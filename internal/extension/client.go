package extension

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

// Event is a worker's acknowledged output. Its consumer must finish writing it
// before returning; that return releases the worker's single stream credit.
type Event struct {
	Name string
	Data json.RawMessage
}

type OutcomeError struct {
	Outcome string
	Cause   error
}

func (e *OutcomeError) Error() string {
	return "extension operation " + e.Outcome
}
func (e *OutcomeError) Unwrap() error { return e.Cause }

type pendingCall struct {
	ctx      context.Context
	scope    sdk.Scope
	frames   chan sdk.Frame
	seq      uint64
	credit   bool
	ended    bool
	hostID   string
	canceled bool
	cancelAt time.Time
}

type workerWrite struct {
	frame sdk.Frame
	done  chan error
}

type workerClient struct {
	group         *componentGroup
	mu            sync.Mutex
	stdin         io.WriteCloser
	stdout        io.ReadCloser
	cancel        context.CancelFunc
	done          chan struct{}
	readDone      chan struct{}
	writeDone     chan struct{}
	logDone       chan struct{}
	pending       map[string]*pendingCall
	next          atomic.Uint64
	slots         chan struct{}
	regularSlots  chan struct{}
	responseSlots chan struct{}
	controlSlots  chan struct{}
	nativeSlots   chan struct{}
	writes        chan workerWrite
	cancelFrames  chan sdk.Frame
	lifetime      context.Context
	hostWG        sync.WaitGroup
	cancelWG      sync.WaitGroup
	err           error
	hostCall      func(context.Context, sdk.Scope, string, json.RawMessage) (any, error)
}

// The child gets ordinary process essentials and its own state/home. In
// particular, environment-based provider credentials are not inherited.
func workerEnv(home string) []string {
	var out []string
	for _, name := range []string{
		"PATH", "SystemRoot", "WINDIR", "COMSPEC", "PATHEXT",
		"TMPDIR", "TMP", "TEMP", "LANG", "LC_ALL", "TZ",
	} {
		if value, ok := os.LookupEnv(name); ok {
			out = append(out, name+"="+value)
		}
	}
	return append(out, "HOME="+home, "USERPROFILE="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, "config"),
		"XDG_CACHE_HOME="+filepath.Join(home, "cache"),
		"APPDATA="+filepath.Join(home, "appdata"))
}

func startWorker(ctx context.Context, root string, entry Installed, bun string,
	hostCall func(context.Context, sdk.Scope, string, json.RawMessage) (any, error),
) (*workerClient, error) {
	directory := filepath.Join(root, entry.Directory)
	program := filepath.Join(directory, filepath.FromSlash(entry.Manifest.Entry))
	args := append([]string(nil), entry.Manifest.Args...)
	if entry.Manifest.Runtime == "bun" {
		if bun == "" {
			var err error
			bun, err = resolveBun(ctx)
			if err != nil {
				return nil, err
			}
		}
		args = append([]string{program}, args...)
		program = bun
	}
	// The handshake deadline does not own a serving module's lifetime.
	lifetime, cancel := context.WithCancel(context.Background())
	var transport *moduleTransport
	var err error
	if entry.Manifest.Runtime == "native" {
		transport, err = openNativeTransport(ctx, program)
	} else {
		var home string
		home, err = workerHome(root, entry.Manifest.ID)
		if err == nil {
			transport, err = openProcessTransport(lifetime, directory, program, args, home)
		}
	}
	if err != nil {
		cancel()
		return nil, err
	}
	stop := func() {
		cancel()
		if transport.stop != nil {
			transport.stop()
		}
	}
	client := &workerClient{
		stdin: transport.input, stdout: transport.output, cancel: stop,
		done: make(chan struct{}), readDone: make(chan struct{}),
		writeDone: make(chan struct{}),
		logDone:   make(chan struct{}), pending: make(map[string]*pendingCall),
		slots: make(chan struct{}, sdk.MaxConcurrent), writes: make(chan workerWrite),
		regularSlots:  make(chan struct{}, sdk.MaxConcurrent/2-2),
		responseSlots: make(chan struct{}, sdk.MaxConcurrent/2),
		controlSlots:  make(chan struct{}, 2),
		nativeSlots:   make(chan struct{}, sdk.MaxConcurrent/2),
		lifetime:      lifetime, hostCall: hostCall,
		cancelFrames: make(chan sdk.Frame, sdk.MaxConcurrent),
	}
	go func() {
		defer close(client.logDone)
		// Drain even an overlong line, without retaining or logging request
		// data a module may have mistakenly printed to stderr.
		_, _ = io.Copy(io.Discard, transport.diagnostics)
	}()
	go client.writeLoop()
	go client.read()
	go func() {
		// StdoutPipe must be drained before Wait closes it. On a protocol
		// failure read cancels the process and releases both pipes.
		<-client.readDone
		err := transport.wait()
		<-client.logDone
		client.fail(err)
		<-client.writeDone
		client.hostWG.Wait()
		client.cancelWG.Wait()
		close(client.done)
	}()
	var actual sdk.Manifest
	result, err := client.call(ctx, "sdk.describe", sdk.Scope{Plugin: entry.Manifest.ID}, nil, nil, nil)
	if err == nil {
		err = json.Unmarshal(result, &actual)
	}
	if err == nil {
		actual.Runtime, actual.Entry, actual.Args = entry.Manifest.Runtime, entry.Manifest.Entry, entry.Manifest.Args
		err = actual.Validate()
	}
	if err == nil {
		declared := publicManifestBytes(actual)
		installed := publicManifestBytes(entry.Manifest)
		if string(declared) != string(installed) {
			err = errors.New("extension handshake does not match its installed contract")
		}
	}
	if err != nil {
		client.close()
		return nil, err
	}
	return client, nil
}

func publicManifestBytes(manifest sdk.Manifest) []byte {
	public := manifest.Public()
	slices.Sort(public.Contract.Capabilities)
	data, _ := json.Marshal(public)
	return data
}

func workerHome(root, id string) (string, error) {
	base, err := NewCatalog(root).open(false)
	if err != nil {
		return "", err
	}
	defer base.Close()
	state, err := catalogPrivateDir(base, "runtime")
	if err != nil {
		return "", err
	}
	defer state.Close()
	module, err := catalogPrivateDir(state, id)
	if err != nil {
		return "", err
	}
	defer module.Close()
	home, err := catalogPrivateDir(module, "home")
	if err != nil {
		return "", err
	}
	defer home.Close()
	return filepath.Abs(home.Name())
}

func (c *workerClient) fail(err error) {
	if err == nil {
		err = io.EOF
	}
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
	c.cancel()
	_ = c.stdin.Close()
	_ = c.stdout.Close()
}

func (c *workerClient) alive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err == nil
}

func (c *workerClient) write(frame sdk.Frame) error {
	ctx, cancel := context.WithTimeout(c.lifetime, 15*time.Second)
	defer cancel()
	return c.send(ctx, frame)
}

func (c *workerClient) send(ctx context.Context, frame sdk.Frame) error {
	work := workerWrite{frame: frame, done: make(chan error, 1)}
	select {
	case c.writes <- work:
	case <-ctx.Done():
		return &OutcomeError{Outcome: "not-started", Cause: ctx.Err()}
	case <-c.lifetime.Done():
		return &OutcomeError{Outcome: "not-started", Cause: errors.New("extension worker stopped")}
	}
	select {
	case err := <-work.done:
		return err
	case <-ctx.Done():
		// A blocked stdin write cannot be abandoned while the process
		// continues: its partial frame could corrupt the next request.
		// A tiny ACK remains serialized by writeLoop and can finish before
		// the queued cancel; it carries no new request or authority.
		if frame.Kind != "ack" {
			c.fail(ctx.Err())
		}
		return ctx.Err()
	case <-c.lifetime.Done():
		return errors.New("extension worker stopped")
	}
}

func (c *workerClient) writeLoop() {
	defer close(c.writeDone)
	for {
		var work workerWrite
		select {
		case frame := <-c.cancelFrames:
			work = workerWrite{frame: frame, done: make(chan error, 1)}
		default:
			select {
			case work = <-c.writes:
			case frame := <-c.cancelFrames:
				work = workerWrite{frame: frame, done: make(chan error, 1)}
			case <-c.lifetime.Done():
				return
			}
		}
		b, err := json.Marshal(work.frame)
		if err != nil || len(b) > sdk.MaxFrameBytes {
			work.done <- &OutcomeError{Outcome: "not-started", Cause: errors.New("extension frame exceeds its contract")}
			continue
		}
		b = append(b, '\n')
		for len(b) > 0 {
			var n int
			n, err = c.stdin.Write(b)
			if err != nil || n == 0 {
				if err == nil {
					err = io.ErrShortWrite
				}
				break
			}
			b = b[n:]
		}
		work.done <- err
		if err != nil {
			c.fail(err)
			return
		}
	}
}

func (c *workerClient) read() {
	defer close(c.readDone)
	reader := bufio.NewScanner(c.stdout)
	reader.Buffer(make([]byte, 64<<10), sdk.MaxFrameBytes+1)
	for reader.Scan() {
		var frame sdk.Frame
		decoder := json.NewDecoder(bytes.NewReader(reader.Bytes()))
		decoder.DisallowUnknownFields()
		if len(reader.Bytes()) > sdk.MaxFrameBytes ||
			decoder.Decode(&frame) != nil || decoder.Decode(new(any)) != io.EOF ||
			frame.ID == "" || len(frame.ID) > 256 {
			c.fail(errors.New("invalid extension response envelope"))
			return
		}
		if frame.Kind == "host.call" {
			if !c.dispatchHost(frame) {
				c.fail(errors.New("invalid scoped host call"))
				return
			}
			continue
		}
		if frame.Kind != "event" && frame.Kind != "result" {
			c.fail(errors.New("unexpected extension response kind"))
			return
		}
		if frame.Method != "" || frame.Params != nil || frame.Auth != nil || frame.Scope != (sdk.Scope{}) ||
			frame.Parent != "" || frame.Deadline != 0 ||
			frame.Kind == "event" && (frame.Event == "" || frame.Seq == 0 || frame.Result != nil || frame.Error != nil) ||
			frame.Kind == "result" && (frame.Event != "" || frame.Seq != 0 || frame.Data != nil ||
				frame.Error != nil && frame.Result != nil) {
			c.fail(errors.New("invalid extension response fields"))
			return
		}
		c.mu.Lock()
		pending := c.pending[frame.ID]
		if pending != nil && frame.Kind == "event" {
			if pending.ended || pending.credit || frame.Seq != pending.seq+1 {
				c.mu.Unlock()
				c.fail(errors.New("invalid extension stream sequence"))
				return
			}
			pending.seq = frame.Seq
			pending.credit = true
		} else if pending != nil {
			if pending.ended || (pending.credit || pending.hostID != "") && !pending.canceled && frame.Error == nil {
				c.mu.Unlock()
				c.fail(errors.New("extension ended before its stream was acknowledged"))
				return
			}
			pending.ended = true
			pending.credit = false
		}
		c.mu.Unlock()
		if pending == nil {
			c.fail(errors.New("extension response has no live call"))
			return
		}
		select {
		case pending.frames <- frame:
		default:
			c.fail(errors.New("extension exceeded stream credit"))
			return
		}
	}
	c.fail(reader.Err())
}

func (c *workerClient) dispatchHost(frame sdk.Frame) bool {
	c.mu.Lock()
	parent := c.pending[frame.Parent]
	valid := parent != nil && !parent.ended && parent.hostID == "" && frame.ID != frame.Parent
	if valid {
		parent.hostID = frame.ID
	}
	c.mu.Unlock()
	if !valid || c.hostCall == nil ||
		(!strings.HasPrefix(frame.Method, "state.") && frame.Method != "network.proxy" &&
			frame.Method != "account.replace" && frame.Method != sdk.MethodComponentCall) {
		return false
	}
	// At most one host callback per live call is admitted by the SDK's
	// protocol. A short callback never holds the global reader or a lock.
	c.hostWG.Add(1)
	go func() {
		defer c.hostWG.Done()
		var result any
		err := parent.ctx.Err()
		if err == nil {
			result, err = c.hostCall(parent.ctx, parent.scope, frame.Method, frame.Params)
		}
		reply := sdk.Frame{Kind: "host.result", ID: frame.ID, Parent: frame.Parent}
		if err != nil {
			reply.Error = &sdk.Error{Code: "host_error", Message: "scoped host operation failed"}
		} else {
			reply.Result, err = json.Marshal(result)
			if err != nil {
				reply.Error = &sdk.Error{Code: "host_error", Message: "invalid host operation result"}
			}
		}
		c.mu.Lock()
		parent.hostID = ""
		ended := parent.ended
		c.mu.Unlock()
		if ended || parent.ctx.Err() != nil {
			return
		}
		_ = c.write(reply)
	}()
	return true
}

func (c *workerClient) call(ctx context.Context, method string, scope sdk.Scope, auth json.RawMessage,
	params any, consume func(Event) error,
) (json.RawMessage, error) {
	class := c.regularSlots
	switch method {
	case sdk.MethodInboundResponse:
		class = c.responseSlots
	case sdk.MethodDescribe, sdk.MethodOperationStatus, sdk.MethodOperationCancel,
		sdk.MethodComponentConfigure, sdk.MethodComponentReady, sdk.MethodComponentStop:
		class = c.controlSlots
	}
	select {
	case class <- struct{}{}:
		defer func() { <-class }()
	case <-ctx.Done():
		return nil, &OutcomeError{Outcome: "not-started", Cause: ctx.Err()}
	case <-c.done:
		return nil, &OutcomeError{Outcome: "not-started", Cause: errors.New("extension worker stopped")}
	}
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-ctx.Done():
		return nil, &OutcomeError{Outcome: "not-started", Cause: ctx.Err()}
	case <-c.done:
		return nil, &OutcomeError{Outcome: "not-started", Cause: errors.New("extension worker stopped")}
	}
	if err := ctx.Err(); err != nil {
		return nil, &OutcomeError{Outcome: "not-started", Cause: err}
	}
	payload, err := json.Marshal(params)
	if err != nil {
		return nil, &OutcomeError{Outcome: "not-started", Cause: err}
	}
	id := strconv.FormatUint(c.next.Add(1), 10)
	pending := &pendingCall{ctx: ctx, scope: scope, frames: make(chan sdk.Frame, 2)}
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return nil, &OutcomeError{Outcome: "not-started", Cause: errors.New("extension worker stopped")}
	}
	c.pending[id] = pending
	c.mu.Unlock()
	retained := false
	defer func() {
		if retained {
			return
		}
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	frame := sdk.Frame{Kind: "call", ID: id, Method: method, Scope: scope, Auth: auth, Params: payload}
	if deadline, ok := ctx.Deadline(); ok {
		frame.Deadline = deadline.UnixMilli()
	}
	if err := c.send(ctx, frame); err != nil {
		var outcome *OutcomeError
		if errors.As(err, &outcome) {
			return nil, err
		}
		return nil, &OutcomeError{Outcome: "uncertain", Cause: err}
	}
	for {
		var reply sdk.Frame
		select {
		case reply = <-pending.frames:
		case <-ctx.Done():
			// The worker cannot keep an unresponsive canceled operation
			// forever. Retire it after a bounded grace period.
			retained = c.cancelPending(id, pending)
			return nil, ctx.Err()
		case <-c.readDone:
			select {
			case reply = <-pending.frames:
			default:
				return nil, &OutcomeError{Outcome: "uncertain", Cause: errors.New("extension worker exited")}
			}
		}
		if reply.Kind == "event" {
			if consume != nil {
				if err := consume(Event{Name: reply.Event, Data: reply.Data}); err != nil {
					retained = c.cancelPending(id, pending)
					return nil, err
				}
			}
			c.mu.Lock()
			pending.credit = false
			ended := pending.ended
			c.mu.Unlock()
			if ended {
				continue
			}
			if err := c.send(ctx, sdk.Frame{Kind: "ack", ID: id, Seq: reply.Seq}); err != nil {
				if ctx.Err() != nil {
					retained = c.cancelPending(id, pending)
					return nil, ctx.Err()
				}
				return nil, &OutcomeError{Outcome: "uncertain", Cause: err}
			}
			continue
		}
		if reply.Error != nil {
			return nil, reply.Error
		}
		if len(reply.Result) > sdk.MaxFrameBytes || !json.Valid(reply.Result) {
			return nil, errors.New("invalid extension result")
		}
		return reply.Result, nil
	}
}

func (c *workerClient) cancelPending(id string, pending *pendingCall) bool {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return false
	}
	pending.canceled = true
	pending.cancelAt = time.Now()
	c.cancelWG.Add(1)
	c.mu.Unlock()
	select {
	case c.cancelFrames <- sdk.Frame{Kind: "cancel", ID: id}:
	default:
		c.fail(errors.New("extension cancellation queue exceeded its bound"))
	}
	go func() {
		defer c.cancelWG.Done()
		c.retireCanceled(id, pending)
	}()
	return true
}

func (c *workerClient) retireCanceled(id string, pending *pendingCall) {
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	timer := time.NewTimer(time.Until(pending.cancelAt.Add(2 * time.Second)))
	defer timer.Stop()
	for {
		select {
		case frame := <-pending.frames:
			if frame.Kind == "result" {
				return
			}
		case <-c.readDone:
			return
		case <-timer.C:
			c.fail(fmt.Errorf("extension did not end canceled call %s", id))
			return
		}
	}
}

func (c *workerClient) close() {
	c.cancel()
	_ = c.stdin.Close()
	_ = c.stdout.Close()
	<-c.done
}
