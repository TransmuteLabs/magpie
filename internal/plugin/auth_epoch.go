package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"sync"
)

const authEpochField = "_magpieAuthEpoch"

var authEpochMemo struct {
	sync.Mutex
	digest [sha256.Size]byte
	epochs map[string]string
}

var errAuthEpochChanged = errors.New("the saved plugin sign-in has changed")

// CurrentAuthEpochs projects current saved slots for a usage check. A
// legacy slot has an empty epoch; an absent slot has no entry. A recovered
// sign-in can run, but its backup cannot confirm the latest epoch.
func CurrentAuthEpochs(ctx context.Context) (map[string]string, error) {
	epochs, err := currentAuthEpochs(ctx, AuthPath())
	return maps.Clone(epochs), err
}

// WithAuthEpoch admits a local publication while the selected sign-in
// still matches. publish runs under the auth writer lock.
func WithAuthEpoch(ctx context.Context, account, epoch string, publish func() error) error {
	path := AuthPath()
	release, err := lockAuth(ctx, path)
	if err != nil {
		return err
	}
	defer release()
	epochs, err := currentAuthEpochs(ctx, path)
	if err != nil {
		return err
	}
	if now, ok := epochs[account]; !ok || now != epoch {
		return errAuthEpochChanged
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return publish()
}

func currentAuthEpochs(ctx context.Context, path string) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := readAuthDocument(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(raw)
	authEpochMemo.Lock()
	defer authEpochMemo.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if authEpochMemo.epochs != nil && authEpochMemo.digest == digest {
		return authEpochMemo.epochs, nil
	}
	var records map[string]json.RawMessage
	if json.Unmarshal(raw, &records) != nil || records == nil {
		return nil, errors.New("current saved plugin sign-ins are unreadable")
	}
	epochs := make(map[string]string, len(records))
	for account, record := range records {
		var fields struct {
			Epoch json.RawMessage `json:"_magpieAuthEpoch"`
		}
		if record = bytes.TrimSpace(record); len(record) == 0 || record[0] != '{' || json.Unmarshal(record, &fields) != nil {
			continue
		}
		var epoch string
		if len(fields.Epoch) > 0 && (json.Unmarshal(fields.Epoch, &epoch) != nil || epoch == "" || len(epoch) > 128) {
			continue
		}
		epochs[account] = epoch
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	authEpochMemo.digest, authEpochMemo.epochs = digest, epochs
	return epochs, nil
}

func authCredentials(raw json.RawMessage) (json.RawMessage, error) {
	if !bytes.Contains(raw, []byte(authEpochField)) && !bytes.ContainsRune(raw, '\\') {
		return raw, nil
	}
	var record map[string]json.RawMessage
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, err
	}
	if _, marked := record[authEpochField]; !marked {
		return raw, nil
	}
	delete(record, authEpochField)
	return json.Marshal(record)
}
