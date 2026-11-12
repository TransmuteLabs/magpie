package plugin

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/yetone/magpie/internal/lastgood"
	"github.com/yetone/magpie/internal/steady"
)

// authLockFrame is the private host-initiated request and response, separate
// from the ids of ordinary Core-to-host calls.
type authLockFrame struct {
	ID     int64           `json:"id"`
	Op     string          `json:"op,omitempty"`
	Token  string          `json:"token,omitempty"`
	All    json.RawMessage `json:"all,omitempty"`
	Known  bool            `json:"known,omitempty"`
	Reseed bool            `json:"reseed,omitempty"`
	Error  string          `json:"error,omitempty"`
}

type authLeaseSnapshot struct {
	file, backup                []byte
	exists, backupExists, known bool
	reseed                      bool
}

func validHostAuth(b []byte) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(b, &m) == nil && m != nil
}

// readAuthLease reads the exact source under the lock. The original bytes
// are also retained to reject manual changes before publishing a proposal.
func readAuthLease(path string, reseed bool) (authLeaseSnapshot, json.RawMessage, error) {
	var s authLeaseSnapshot
	b, err := steady.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		s.known = true
		return s, json.RawMessage(`{}`), nil
	}
	if err != nil {
		return s, nil, err
	}
	s.exists, s.file = true, b
	if validHostAuth(b) {
		s.known = true
		return s, b, nil
	}
	bak, err := steady.ReadFile(lastgood.Bak(path))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return s, nil, err
	}
	if err == nil {
		s.backupExists, s.backup = true, bak
	}
	if !validHostAuth(bak) {
		if !reseed {
			return s, nil, errors.New("plugin sign-in change not made: auth and backup are unreadable")
		}
		s.reseed = true
		return s, json.RawMessage(`{}`), nil
	}
	// Fallback records the same recovery notice as other auth readers.
	recovered, err := lastgood.Fallback(path, b, validHostAuth)
	if err != nil {
		return s, nil, err
	}
	if !bytes.Equal(recovered, bak) {
		return s, nil, errors.New("plugin sign-in change not made: auth backup changed")
	}
	s.known = true
	return s, recovered, nil
}

func sameAuthFile(path string, present bool, want []byte) (bool, error) {
	b, err := steady.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return !present, nil
	}
	if err != nil {
		return false, err
	}
	return present && bytes.Equal(b, want), nil
}

func checkAuthLeaseSource(path string, base authLeaseSnapshot) error {
	match, err := sameAuthFile(path, base.exists, base.file)
	if err != nil {
		return err
	}
	if match && base.exists && !validHostAuth(base.file) {
		match, err = sameAuthFile(lastgood.Bak(path), base.backupExists, base.backup)
		if err != nil {
			return err
		}
	}
	if !match {
		return errors.New("plugin sign-in change not made: auth changed while editing")
	}
	return nil
}

func publishAuthLease(path string, base authLeaseSnapshot, proposal json.RawMessage) error {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(proposal, &all); err != nil || all == nil {
		return errors.New("plugin sign-in change not made: invalid auth proposal")
	}
	if !base.known && (!base.reseed || len(all) != 1) {
		return errors.New("plugin sign-in change not made: auth is unreadable")
	}
	if err := checkAuthLeaseSource(path, base); err != nil {
		return err
	}
	var formatted bytes.Buffer
	if err := json.Indent(&formatted, proposal, "", "  "); err != nil {
		return err
	}
	formatted.WriteByte('\n')
	if err := lastgood.Keep(path, validHostAuth, 0o600); err != nil {
		return err
	}
	if err := checkAuthLeaseSource(path, base); err != nil {
		return err
	}
	if err := writeWhole(path, formatted.Bytes()); err != nil {
		return err
	}
	// Even if flushing the directory fails, the rename may be visible.
	changed()
	if runtime.GOOS != "windows" {
		dir, err := os.Open(filepath.Dir(path))
		if err != nil {
			return err
		}
		err = dir.Sync()
		if closeErr := dir.Close(); err == nil {
			err = closeErr
		}
		return err
	}
	return nil
}

func (h *host) authLockReply(frame authLockFrame) {
	_ = h.enqueue(h.ctx, struct {
		AuthLock authLockFrame `json:"authLock"`
	}{frame})
}

func (h *host) authLockRequest(req authLockFrame) {
	if req.ID <= 0 {
		return
	}
	reply := authLockFrame{ID: req.ID}
	h.authMu.Lock()
	switch req.Op {
	case "acquire":
		if h.authExited || h.authAcquiring || h.authUnlock != nil {
			h.authMu.Unlock()
			reply.Error = "plugin sign-in change not made: auth lock already requested or host exited"
			go h.authLockReply(reply)
			return
		}
		h.authAcquiring = true
		h.authMu.Unlock()
		go h.acquireAuthLease(reply, req.Reseed)
	case "abort", "commit":
		if req.Token == "" || req.Token != h.authToken || h.authUnlock == nil || h.authCommitting {
			h.authMu.Unlock()
			reply.Error = "plugin auth lock lease does not belong to this host"
			go h.authLockReply(reply)
			return
		}
		if req.Op == "abort" {
			h.releaseAuthLeaseLocked()
			h.authMu.Unlock()
			go h.authLockReply(reply)
			return
		}
		h.authCommitting = true
		base := h.authBase
		h.authMu.Unlock()
		go h.commitAuthLease(reply, base, req.All)
	default:
		h.authMu.Unlock()
		reply.Error = "unknown plugin auth lock operation"
		go h.authLockReply(reply)
	}
}

func (h *host) acquireAuthLease(reply authLockFrame, reseed bool) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		h.authMu.Lock()
		h.authAcquiring = false
		h.authMu.Unlock()
		reply.Error = "plugin sign-in change not made: could not create auth lock lease"
		h.authLockReply(reply)
		return
	}
	unlock, err := lockAuthAt(h.authPath)
	var base authLeaseSnapshot
	var all json.RawMessage
	if err == nil {
		base, all, err = readAuthLease(h.authPath, reseed)
		if err != nil {
			unlock()
		}
	}
	h.authMu.Lock()
	h.authAcquiring = false
	if err != nil {
		h.authMu.Unlock()
		reply.Error = err.Error()
		h.authLockReply(reply)
		return
	}
	if h.authExited {
		h.authMu.Unlock()
		unlock()
		return
	}
	reply.Token = hex.EncodeToString(random[:])
	reply.All, reply.Known = all, base.known
	h.authToken, h.authUnlock, h.authBase = reply.Token, unlock, base
	h.authMu.Unlock()
	h.authLockReply(reply)
}

func (h *host) commitAuthLease(reply authLockFrame, base authLeaseSnapshot, proposal json.RawMessage) {
	err := publishAuthLease(h.authPath, base, proposal)
	h.authMu.Lock()
	h.authCommitting = false
	h.releaseAuthLeaseLocked()
	h.authMu.Unlock()
	if err != nil {
		reply.Error = err.Error()
	}
	h.authLockReply(reply)
}

func (h *host) releaseAuthLeaseLocked() {
	if h.authUnlock != nil {
		h.authUnlock()
		h.authUnlock = nil
	}
	h.authToken = ""
	h.authBase = authLeaseSnapshot{}
}

// Only a confirmed child exit releases an idle grant. An accepted commit
// keeps ownership until its Go publication has finished, even if the host
// exits while that publication is in progress.
func (h *host) endAuthLease() {
	h.authMu.Lock()
	h.authExited = true
	if !h.authCommitting {
		h.releaseAuthLeaseLocked()
	}
	h.authMu.Unlock()
}
