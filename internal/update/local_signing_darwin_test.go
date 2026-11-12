package update

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type signingFixture struct {
	t          *testing.T
	der        []byte
	id         SigningIdentity
	states     map[string]bool // true: certificate-signed, false: ad-hoc
	broken     map[string]bool
	hashes     map[string]string
	entitled   bool
	missingKey bool
	wrongLeaf  bool
	badDR      bool
	signError  bool
	signs      int
	required   int
}

func fakeSigning(t *testing.T) *signingFixture {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("MAGPIE_UPDATE_FEED", "https://updates.example.test/latest.json")
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Same signing label", OrganizationalUnit: []string{"TEAM"}}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	f := &signingFixture{t: t, der: der, id: SigningIdentity{Fingerprint: certFingerprint(cert), Name: "Same signing label", Team: "TEAM", Expires: cert.NotAfter}, states: map[string]bool{}, broken: map[string]bool{}, hashes: map[string]string{}}
	old := signingRun
	signingRun = f.run
	t.Cleanup(func() { signingRun = old })
	return f
}

func (f *signingFixture) app(name string, signed bool) string {
	f.t.Helper()
	p := filepath.Join(f.t.TempDir(), name+".app")
	if err := os.MkdirAll(filepath.Join(p, "Contents", "MacOS"), 0o700); err != nil {
		f.t.Fatal(err)
	}
	plist := `<?xml version="1.0"?><plist version="1.0"><dict><key>CFBundleIdentifier</key><string>com.example.magpie</string><key>CFBundleExecutable</key><string>magpie</string></dict></plist>`
	if err := os.WriteFile(filepath.Join(p, "Contents", "Info.plist"), []byte(plist), 0o600); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "Contents", "MacOS", "magpie"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		f.t.Fatal(err)
	}
	f.states[p] = signed
	return p
}

func (f *signingFixture) run(ctx context.Context, tool string, args ...string) ([]byte, []byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if tool == "/usr/bin/security" {
		switch args[0] {
		case "find-identity":
			if f.missingKey {
				return []byte("0 valid identities found"), nil, nil
			}
			return []byte(fmt.Sprintf("  1) %s \"Same signing label\"\n  2) %s \"Same signing label\"\n  2 valid identities found\n", f.id.Fingerprint, strings.Repeat("A", 40))), nil, nil
		case "find-certificate":
			return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.der}), nil, nil
		}
	}
	if tool != "/usr/bin/codesign" {
		return nil, nil, fmt.Errorf("unexpected tool: %s", tool)
	}
	p := args[len(args)-1]
	signed := f.states[p]
	joined := strings.Join(args, " ")
	if args[0] == "--force" {
		f.signs++
		if f.signError {
			return nil, []byte("private key denied"), errors.New("exit 1")
		}
		if strings.Contains(joined, "--deep") || strings.Contains(joined, "preserve-metadata") || strings.Contains(joined, "--requirements") {
			f.t.Error("unsafe signing options", args)
		}
		if !strings.Contains(joined, "--sign "+f.id.Fingerprint) {
			f.t.Error("wrong signing identity", args)
		}
		f.states[p] = true
		return nil, nil, nil
	}
	if args[0] == "--verify" {
		if f.broken[p] {
			return nil, []byte("a sealed resource is missing or invalid"), errors.New("exit 1")
		}
		if strings.Contains(joined, " -R ") {
			f.required++
			if f.badDR {
				return nil, nil, errors.New("requirement failed")
			}
		}
		return nil, nil, nil
	}
	if strings.Contains(joined, "--verbose=4") {
		flags, team, sig := "2(adhoc)", "not set", "Signature=adhoc"
		if signed {
			flags, team, sig = "0(none)", "TEAM", "Authority=Same signing label"
		}
		h := strings.Repeat("1", 40)
		if signed {
			h = strings.Repeat("2", 40)
		}
		if f.hashes[p] != "" {
			h = f.hashes[p]
		}
		return nil, []byte(fmt.Sprintf("Identifier=com.example.magpie\nCodeDirectory v=20400 flags=0x%s hashes=10\nCDHash=%s\n%s\nTeamIdentifier=%s\n", flags, h, sig, team)), nil
	}
	if strings.Contains(joined, "--entitlements") {
		if f.entitled {
			return []byte("<plist><dict/></plist>"), nil, nil
		}
		return nil, nil, nil
	}
	if strings.Contains(joined, "--requirements") {
		r := `identifier "com.example.magpie" and certificate leaf[subject.OU] = "TEAM"`
		if !signed {
			r = `cdhash H"` + strings.Repeat("1", 40) + `"`
		}
		if f.badDR && signed {
			r += ` and cdhash H"` + strings.Repeat("2", 40) + `"`
		}
		return []byte("designated => " + r + "\n"), nil, nil
	}
	if strings.Contains(joined, "--extract-certificates") {
		for i, a := range args {
			if a == "--extract-certificates" {
				b := f.der
				if f.wrongLeaf {
					b = []byte("not a certificate")
				}
				return nil, nil, os.WriteFile(args[i+1]+"0", b, 0o600)
			}
		}
	}
	return nil, nil, fmt.Errorf("unexpected codesign call %v", args)
}

func (f *signingFixture) enroll(app string) *SigningConfig {
	f.t.Helper()
	if err := UseSigning(context.Background(), f.id.Fingerprint, app); err != nil {
		f.t.Fatal(err)
	}
	c, err := Signing()
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func TestLocalSigningBootstrapAndStableRequirement(t *testing.T) {
	f := fakeSigning(t)
	old, next := f.app("old", false), f.app("next", false)
	c := f.enroll(old)
	if f.signs != 0 {
		t.Fatal("enrollment signed the live app")
	}
	installed, payload, err := signLocalApp(context.Background(), c, old, next)
	if err != nil {
		t.Fatal(err)
	}
	if !installed.Adhoc || payload.Adhoc || payload.Fingerprint != c.Fingerprint || f.signs != 1 {
		t.Fatalf("bad bootstrap: %+v %+v", installed, payload)
	}
	// A subsequent update uses the installed certificate requirement, not the
	// old ad-hoc cdhash requirement and not only a matching team name.
	third := f.app("third", false)
	if _, _, err := signLocalApp(context.Background(), c, next, third); err != nil {
		t.Fatal(err)
	}
	if f.required == 0 {
		t.Fatal("installed designated requirement was not checked")
	}
}

func TestLocalSigningRefusesBeforeSigning(t *testing.T) {
	for _, kind := range []string{"corrupt", "missing key", "denied key", "other bootstrap", "entitlements", "symlink", "nested code", "feed changed", "foreign release"} {
		t.Run(kind, func(t *testing.T) {
			f := fakeSigning(t)
			old, next := f.app("old", false), f.app("next", false)
			c := f.enroll(old)
			rel := &Release{Version: "0.1.1", originFeed: c.Feed}
			switch kind {
			case "corrupt":
				f.broken[next] = true
			case "missing key":
				f.missingKey = true
			case "denied key":
				f.signError = true
			case "other bootstrap":
				f.hashes[old] = strings.Repeat("9", 40)
			case "entitlements":
				f.entitled = true
			case "symlink":
				if err := os.Symlink("Info.plist", filepath.Join(next, "Contents", "link")); err != nil {
					t.Fatal(err)
				}
			case "nested code":
				if err := os.Mkdir(filepath.Join(next, "Contents", "helper.app"), 0o700); err != nil {
					t.Fatal(err)
				}
			case "feed changed":
				t.Setenv("MAGPIE_UPDATE_FEED", "https://other.test/latest")
			case "foreign release":
				rel.originFeed = "https://other.test/latest"
			}
			err := prepareSigning(context.Background(), rel, old, next, c)
			if err == nil {
				t.Fatal("accepted invalid signing state")
			}
			want := 0
			if kind == "denied key" {
				want = 1
			}
			if f.signs != want {
				t.Fatalf("private key used %d times, want %d", f.signs, want)
			}
		})
	}
}

func TestLocalSigningReceiptRejectsStaleAndTamperedInstalls(t *testing.T) {
	for _, kind := range []string{"no receipt", "policy changed", "policy off", "payload changed", "installed changed", "broken payload", "wrong leaf"} {
		t.Run(kind, func(t *testing.T) {
			f := fakeSigning(t)
			old, next := f.app("old", false), f.app("next", false)
			c := f.enroll(old)
			if err := prepareSigning(context.Background(), &Release{Version: "0.1.1", originFeed: c.Feed}, old, next, c); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "no receipt":
				if err := os.Remove(receiptPath(next)); err != nil {
					t.Fatal(err)
				}
			case "policy changed":
				f.enroll(old)
			case "policy off":
				if err := DisableSigning(); err != nil {
					t.Fatal(err)
				}
			case "payload changed":
				f.hashes[next] = strings.Repeat("9", 40)
			case "installed changed":
				f.hashes[old] = strings.Repeat("9", 40)
			case "broken payload":
				f.broken[next] = true
			case "wrong leaf":
				f.wrongLeaf = true
			}
			for _, install := range []func(string, string) error{Install, InstallAsAdmin} {
				if err := install(next, old); !errors.Is(err, ErrSigningStale) {
					t.Fatalf("install accepted stale update: %v", err)
				}
				if _, err := os.Stat(old); err != nil {
					t.Fatalf("live app changed: %v", err)
				}
				if _, err := os.Stat(next); err != nil {
					t.Fatalf("staged app changed: %v", err)
				}
			}
			if f.signs != 1 {
				t.Fatal("installation used private key")
			}
		})
	}
}

func TestLocalSigningReceiptPermitsVerifiedInstall(t *testing.T) {
	f := fakeSigning(t)
	old, next := f.app("old", false), f.app("next", false)
	c := f.enroll(old)
	if err := prepareSigning(context.Background(), &Release{Version: "0.1.1", originFeed: c.Feed}, old, next, c); err != nil {
		t.Fatal(err)
	}
	if err := Install(next, old); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(next); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("staging not moved", err)
	}
}

func TestLocalSigningConsumesBootstrapAfterInstall(t *testing.T) {
	f := fakeSigning(t)
	old, next := f.app("old", false), f.app("next", false)
	c := f.enroll(old)
	if err := prepareSigning(context.Background(), &Release{Version: "0.1.1", originFeed: c.Feed}, old, next, c); err != nil {
		t.Fatal(err)
	}
	if err := Install(next, old); err != nil {
		t.Fatal(err)
	}
	current, err := Signing()
	if err != nil {
		t.Fatal(err)
	}
	if current.Bootstrap != nil {
		t.Fatal("bootstrap enrollment survived successful installation")
	}
	if err := authorizeInstalled(current, *c.Bootstrap); err == nil {
		t.Fatal("bootstrap could be reused")
	}
}

func TestLocalSigningPolicyErrorsAreNotOff(t *testing.T) {
	f := fakeSigning(t)
	old := f.app("old", false)
	f.enroll(old)
	s, err := os.Stat(signingPath())
	if err != nil {
		t.Fatal(err)
	}
	if s.Mode().Perm() != 0o600 {
		t.Fatal("policy permissions", s.Mode())
	}
	if err := os.WriteFile(signingPath(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Signing(); err == nil {
		t.Fatal("malformed policy treated as off")
	}
	if err := Install(old, old); err == nil {
		t.Fatal("malformed policy accepted at installation")
	}
}

func TestLocalSigningStageChecksDownloadBeforeKeyUse(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprint("corrupt=", corrupt), func(t *testing.T) {
			f := fakeSigning(t)
			old, source := f.app("old", false), f.app("source", false)
			var data bytes.Buffer
			w := zip.NewWriter(&data)
			for _, name := range []string{"Contents/Info.plist", "Contents/MacOS/magpie"} {
				b, err := os.ReadFile(filepath.Join(source, name))
				if err != nil {
					t.Fatal(err)
				}
				h := &zip.FileHeader{Name: "magpie.app/" + name, Method: zip.Deflate}
				h.SetMode(0o644)
				if strings.HasSuffix(name, "/magpie") {
					h.SetMode(0o755)
				}
				out, err := w.CreateHeader(h)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := out.Write(b); err != nil {
					t.Fatal(err)
				}
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256(data.Bytes())
			var rel Release
			server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/latest" {
					_ = json.NewEncoder(rw).Encode(rel)
					return
				}
				_, _ = rw.Write(data.Bytes())
			}))
			t.Cleanup(server.Close)
			t.Setenv("MAGPIE_UPDATE_FEED", server.URL+"/latest")
			f.enroll(old)
			rel = Release{Version: "0.1.2", Assets: map[string]Asset{AppAsset(): {URL: server.URL + "/app.zip", Size: int64(data.Len()), SHA256: fmt.Sprintf("%x", hash)}}}
			if corrupt {
				a := rel.Assets[AppAsset()]
				a.SHA256 = strings.Repeat("0", 64)
				rel.Assets[AppAsset()] = a
			}
			latest, err := Latest(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			staged, err := Stage(context.Background(), latest, old)
			if corrupt {
				if err == nil || f.signs != 0 {
					t.Fatalf("corrupt archive reached signing: %v, %d", err, f.signs)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if f.signs != 1 {
				t.Fatal("staging did not sign", f.signs)
			}
			if _, err := os.Stat(receiptPath(staged)); err != nil {
				t.Fatal("missing receipt", err)
			}
		})
	}
}

func TestLocalSigningIdentitiesSelectFullFingerprint(t *testing.T) {
	f := fakeSigning(t)
	ids, err := SigningIdentities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0].Fingerprint != f.id.Fingerprint {
		t.Fatal("did not match public certificate to private-key identity", ids)
	}
	if err := UseSigning(context.Background(), strings.Repeat("A", 40), f.app("old", false)); err == nil {
		t.Fatal("same label accepted instead of selected certificate")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := SigningIdentities(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("cancel lost", err)
	}
}
