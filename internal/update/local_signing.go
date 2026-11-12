package update

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/filelock"
)

// SigningConfig is local to this computer. It is deliberately outside Settings
// and backup.Collect: another computer does not have this private key.
type SigningConfig struct {
	Version     int              `json:"version"`
	Revision    string           `json:"revision"`
	Fingerprint string           `json:"fingerprint"`
	Team        string           `json:"team"`
	BundleID    string           `json:"bundleID"`
	Feed        string           `json:"feed"`
	Bootstrap   *signingSnapshot `json:"bootstrap,omitempty"`
}

// SigningIdentity names a certificate whose private key is in the keychain.
// Fingerprint, rather than Name or Team, selects the key.
type SigningIdentity struct {
	Fingerprint string
	Name        string
	Team        string
	Expires     time.Time
}

type signingSnapshot struct {
	Identifier  string   `json:"identifier"`
	Team        string   `json:"team,omitempty"`
	Fingerprint string   `json:"fingerprint,omitempty"`
	Requirement string   `json:"requirement"`
	Hashes      []string `json:"hashes"`
	Adhoc       bool     `json:"adhoc"`
}

type signingReceipt struct {
	Version   int             `json:"version"`
	Policy    string          `json:"policy"`
	Feed      string          `json:"feed"`
	Installed signingSnapshot `json:"installed"`
	Payload   signingSnapshot `json:"payload"`
}

// ErrSigningStale means a cached download cannot be installed under the current
// signing policy. Callers must prepare a new download, not ask for elevation.
var ErrSigningStale = errors.New("local signing policy or staged app changed; download the update again")

func signingPath() string { return filepath.Join(appdir.Config(), "update-signing.json") }
func receiptPath(staged string) string {
	return filepath.Join(filepath.Dir(staged), "local-signing.json")
}

// Signing returns the saved policy, or nil when local signing is disabled.
// An unreadable or malformed policy is never treated as disabled.
func Signing() (*SigningConfig, error) {
	var c SigningConfig
	if err := readSigningJSON(signingPath(), &c); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("local update signing: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("local update signing: %w", err)
	}
	return &c, nil
}

func fingerprint(s string) (string, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 20 {
		return "", errors.New("use the complete 40-hex code-signing certificate fingerprint")
	}
	return s, nil
}

func (c SigningConfig) validate() error {
	fp, err := fingerprint(c.Fingerprint)
	if err != nil || fp != c.Fingerprint || c.Version != 1 || c.Team == "" || c.BundleID == "" {
		return errors.New("invalid update-signing.json identity or format")
	}
	r, err := hex.DecodeString(c.Revision)
	if err != nil || len(r) != 16 {
		return errors.New("invalid update-signing.json revision")
	}
	if err := signingFeed(c.Feed); err != nil {
		return err
	}
	if c.Bootstrap != nil && (!c.Bootstrap.Adhoc || c.Bootstrap.Identifier != c.BundleID || len(c.Bootstrap.Hashes) == 0) {
		return errors.New("invalid local signing bootstrap")
	}
	return nil
}

func signingFeed(feed string) error {
	u, err := url.Parse(feed)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || feed == siteFeed {
		return errors.New("local signing requires an explicitly configured custom MAGPIE_UPDATE_FEED without URL credentials")
	}
	return nil
}

func readSigningJSON(path string, out any) error {
	s, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !s.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func writeSigningJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		if err := f.Close(); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrExist) {
		return err
	}
	s, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !s.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	return edit.WriteAtomic(path, append(b, '\n'))
}

func signingLock() (func(), error) {
	if runtime.GOOS != "darwin" {
		return func() {}, nil
	}
	unlock, held := filelock.Lock(signingPath()+".lock", filelock.Wait)
	if !held {
		return nil, errors.New("cannot lock update signing; another update may still be running")
	}
	return unlock, nil
}

// DisableSigning explicitly disables the policy. Already signed downloads must
// be staged again; their receipts are not accepted with a different policy.
func DisableSigning() error {
	unlock, err := signingLock()
	if err != nil {
		return err
	}
	defer unlock()
	if err := os.Remove(signingPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func policyDigest(c *SigningConfig) string {
	b, _ := json.Marshal(c)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func newSigningConfig(id SigningIdentity, installed signingSnapshot, feed string) (*SigningConfig, error) {
	var revision [16]byte
	if _, err := rand.Read(revision[:]); err != nil {
		return nil, err
	}
	c := &SigningConfig{Version: 1, Revision: hex.EncodeToString(revision[:]), Fingerprint: id.Fingerprint,
		Team: id.Team, BundleID: installed.Identifier, Feed: feed}
	if installed.Adhoc {
		c.Bootstrap = &installed
	} else if err := matchesSigningPin(c, installed); err != nil {
		return nil, err
	}
	return c, c.validate()
}

func matchesSigningPin(c *SigningConfig, s signingSnapshot) error {
	if s.Adhoc || s.Fingerprint != c.Fingerprint || s.Team != c.Team || s.Identifier != c.BundleID {
		return errors.New("app certificate, team or bundle identifier differs from the pinned signing identity")
	}
	if s.Requirement == "" || strings.Contains(strings.ToLower(s.Requirement), "cdhash") {
		return errors.New("the signed app has no stable designated requirement")
	}
	return nil
}

func authorizeInstalled(c *SigningConfig, s signingSnapshot) error {
	if s.Adhoc {
		if c.Bootstrap == nil || !reflect.DeepEqual(s, *c.Bootstrap) {
			return errors.New("the installed ad-hoc app differs from the enrolled bootstrap app; select the signing identity again")
		}
		return nil
	}
	return matchesSigningPin(c, s)
}

func prepareSigning(ctx context.Context, rel *Release, bundle, app string, c *SigningConfig) error {
	if c == nil {
		return sameSigner(ctx, bundle, app)
	}
	if os.Getenv("MAGPIE_UPDATE_FEED") != c.Feed || Feed() != c.Feed || rel.originFeed != c.Feed {
		return errors.New("the release was not obtained from the pinned update feed; check for updates again")
	}
	if err := rel.verify(); err != nil {
		return err
	}
	installed, payload, err := signLocalApp(ctx, c, bundle, app)
	if err != nil {
		return err
	}
	current, err := Signing()
	if err != nil {
		return err
	}
	if current == nil || policyDigest(current) != policyDigest(c) || Feed() != c.Feed {
		return ErrSigningStale
	}
	return writeSigningJSON(receiptPath(app), signingReceipt{Version: 1, Policy: policyDigest(c), Feed: c.Feed, Installed: installed, Payload: payload})
}

// finishSigningBootstrap consumes the one-time ad-hoc enrollment only after the
// verified replacement succeeded. Failure is reported without retrying an
// already completed swap through the administrator path.
func finishSigningBootstrap() error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	c, err := Signing()
	if err != nil {
		return fmt.Errorf("%w: update installed, but signing policy could not be read: %v", ErrSigningStale, err)
	}
	if c == nil || c.Bootstrap == nil {
		return nil
	}
	c.Bootstrap = nil
	if err := writeSigningJSON(signingPath(), c); err != nil {
		return fmt.Errorf("%w: update installed, but bootstrap enrollment could not be consumed: %v", ErrSigningStale, err)
	}
	return nil
}

// Called with signingLock held, before either installation entry point mutates
// any bundle. It verifies only: private keys are never used during installation.
func signingPreflight(staged, bundle string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	c, err := Signing()
	if err != nil {
		return err
	}
	var r signingReceipt
	err = readSigningJSON(receiptPath(staged), &r)
	if c == nil && errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil || c == nil || r.Version != 1 || r.Policy != policyDigest(c) || r.Feed != c.Feed || Feed() != c.Feed {
		return ErrSigningStale
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	installed, payload, err := verifyLocalPair(ctx, c, bundle, staged)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSigningStale, err)
	}
	if !reflect.DeepEqual(installed, r.Installed) || !reflect.DeepEqual(payload, r.Payload) {
		return ErrSigningStale
	}
	return nil
}
