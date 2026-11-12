package update

import (
	"bytes"
	"context"
	"crypto/sha1" // Keychain's codesign identity selector is the certificate's SHA-1.
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/lazyre"
	"github.com/yetone/magpie/internal/proc"
)

// Separate stdout (PEM/plist) from diagnostics; no shell or key export. Tests
// replace this runner and never access the user's keychain.
var signingRun = func(ctx context.Context, tool string, args ...string) ([]byte, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := proc.CommandContext(ctx, tool, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func signingCommand(ctx context.Context, tool string, args ...string) ([]byte, []byte, error) {
	out, diagnostic, err := signingRun(ctx, tool, args...)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, fmt.Errorf("%s: %w: %s", filepath.Base(tool), err, strings.TrimSpace(string(diagnostic)))
	}
	return out, diagnostic, nil
}

var identityLine = lazyre.New(`(?m)^\s*\d+\) ([A-Fa-f0-9]{40}) "([^"]+)"\s*$`)

// SigningIdentities lists usable code-signing identities without accessing their
// private keys. Expired or unsupported public certificates are not offered.
func SigningIdentities(ctx context.Context) ([]SigningIdentity, error) {
	out, _, err := signingCommand(ctx, "/usr/bin/security", "find-identity", "-v", "-p", "codesigning")
	if err != nil {
		return nil, err
	}
	wanted := map[string]string{}
	for _, m := range identityLine.FindAllSubmatch(out, -1) {
		wanted[strings.ToUpper(string(m[1]))] = string(m[2])
	}
	if len(wanted) == 0 {
		return nil, nil
	}
	out, _, err = signingCommand(ctx, "/usr/bin/security", "find-certificate", "-a", "-p")
	if err != nil {
		return nil, err
	}
	ids := map[string]SigningIdentity{}
	for len(bytes.TrimSpace(out)) > 0 {
		block, rest := pem.Decode(out)
		if block == nil {
			return nil, errors.New("cannot decode public keychain certificates")
		}
		out = rest
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("public signing certificate: %w", err)
		}
		fp := certFingerprint(cert)
		name, ok := wanted[fp]
		if !ok {
			continue
		}
		if err := signingCertificate(cert); err != nil {
			continue
		}
		ids[fp] = SigningIdentity{Fingerprint: fp, Name: name, Team: cert.Subject.OrganizationalUnit[0], Expires: cert.NotAfter}
	}
	result := make([]SigningIdentity, 0, len(ids))
	for _, id := range ids {
		result = append(result, id)
	}
	slices.SortFunc(result, func(a, b SigningIdentity) int { return strings.Compare(a.Fingerprint, b.Fingerprint) })
	return result, nil
}

func certFingerprint(cert *x509.Certificate) string {
	h := sha1.Sum(cert.Raw)
	return strings.ToUpper(hex.EncodeToString(h[:]))
}

func signingCertificate(cert *x509.Certificate) error {
	now := time.Now()
	if now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
		return errors.New("the signing certificate is expired or not yet valid")
	}
	if len(cert.Subject.OrganizationalUnit) != 1 || cert.Subject.OrganizationalUnit[0] == "" ||
		!slices.Contains(cert.ExtKeyUsage, x509.ExtKeyUsageCodeSigning) {
		return errors.New("the signing certificate has no unambiguous team or code-signing purpose")
	}
	return nil
}

func findSigningIdentity(ctx context.Context, fp string) (SigningIdentity, error) {
	ids, err := SigningIdentities(ctx)
	if err != nil {
		return SigningIdentity{}, err
	}
	for _, id := range ids {
		if id.Fingerprint == fp {
			return id, nil
		}
	}
	return SigningIdentity{}, fmt.Errorf("pinned signing identity %s is unavailable or expired; unlock its keychain or explicitly select a certificate", fp)
}

// UseSigning enrolls the current installed bundle. It does not sign it or touch
// a private key. An ad-hoc enrollment authorizes exactly this bootstrap app.
func UseSigning(ctx context.Context, fp, bundle string) error {
	fp, err := fingerprint(fp)
	if err != nil {
		return err
	}
	feed := os.Getenv("MAGPIE_UPDATE_FEED")
	if err := signingFeed(feed); err != nil {
		return err
	}
	if bundle == "" {
		return errors.New("local signing must be configured from the installed macOS app bundle")
	}
	unlock, err := signingLock()
	if err != nil {
		return err
	}
	defer unlock()
	id, err := findSigningIdentity(ctx, fp)
	if err != nil {
		return err
	}
	installed, err := inspectSigningApp(ctx, bundle)
	if err != nil {
		return err
	}
	c, err := newSigningConfig(id, installed, feed)
	if err != nil {
		return err
	}
	return writeSigningJSON(signingPath(), c)
}

// Strictly parse the bundle plist, including duplicate keys and types. Defaults
// suitable for launching an app are not suitable for authorizing its signature.
func signingBundleID(path string) (string, error) {
	b, err := os.ReadFile(filepath.Join(path, "Contents", "Info.plist"))
	if err != nil {
		return "", err
	}
	var p struct {
		XMLName xml.Name `xml:"plist"`
		Dict    struct {
			Items []struct {
				XMLName xml.Name
				Value   string `xml:",chardata"`
			} `xml:",any"`
		} `xml:"dict"`
	}
	if err := xml.Unmarshal(b, &p); err != nil {
		return "", fmt.Errorf("bundle Info.plist: %w", err)
	}
	values := map[string]string{}
	for i := 0; i < len(p.Dict.Items); i += 2 {
		if i+1 >= len(p.Dict.Items) || p.Dict.Items[i].XMLName.Local != "key" {
			return "", errors.New("unsupported bundle Info.plist dictionary")
		}
		key, value := p.Dict.Items[i], p.Dict.Items[i+1]
		if key.Value != "CFBundleIdentifier" && key.Value != "CFBundleExecutable" {
			continue
		}
		if _, exists := values[key.Value]; exists || value.XMLName.Local != "string" {
			return "", errors.New("ambiguous bundle identifier or executable")
		}
		values[key.Value] = strings.TrimSpace(value.Value)
	}
	if values["CFBundleIdentifier"] == "" || values["CFBundleExecutable"] != "magpie" {
		return "", errors.New("local signing supports only a bundle with an identifier and executable magpie")
	}
	return values["CFBundleIdentifier"], nil
}

func signingLayout(path string) (string, error) {
	s, err := os.Lstat(path)
	if err != nil || !s.IsDir() || s.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("local signing requires a real app directory")
	}
	id, err := signingBundleID(path)
	if err != nil {
		return "", err
	}
	main := filepath.Join(path, "Contents", "MacOS", "magpie")
	found := false
	err = filepath.WalkDir(path, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("local signing does not support symlinks: %s", p)
		}
		if d.IsDir() {
			if p != path && (strings.HasSuffix(d.Name(), ".app") || strings.HasSuffix(d.Name(), ".framework") || strings.HasSuffix(d.Name(), ".xpc") || strings.HasSuffix(d.Name(), ".appex")) {
				return fmt.Errorf("local signing does not support nested code: %s", p)
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported bundle file: %s", p)
		}
		if p == main {
			found = true
			if info.Mode().Perm()&0o111 == 0 {
				return errors.New("the bundle executable is not executable")
			}
			return nil
		}
		if info.Mode().Perm()&0o111 != 0 {
			return fmt.Errorf("local signing does not support additional executables: %s", p)
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		var magic [4]byte
		n, readErr := io.ReadFull(f, magic[:])
		closeErr := f.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		if n == 4 && (bytes.Equal(magic[:], []byte{0xfe, 0xed, 0xfa, 0xce}) || bytes.Equal(magic[:], []byte{0xce, 0xfa, 0xed, 0xfe}) || bytes.Equal(magic[:], []byte{0xfe, 0xed, 0xfa, 0xcf}) || bytes.Equal(magic[:], []byte{0xcf, 0xfa, 0xed, 0xfe}) || bytes.Equal(magic[:], []byte{0xca, 0xfe, 0xba, 0xbe}) || bytes.Equal(magic[:], []byte{0xbe, 0xba, 0xfe, 0xca}) || bytes.HasPrefix(magic[:], []byte("#!"))) {
			return fmt.Errorf("local signing does not support nested code: %s", p)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if !found {
		return "", errors.New("the bundle executable is missing")
	}
	return id, nil
}

var codeFlags = lazyre.New(`flags=0x([0-9a-fA-F]+)`)

func inspectSigningApp(ctx context.Context, path string) (signingSnapshot, error) {
	var result signingSnapshot
	id, err := signingLayout(path)
	if err != nil {
		return result, err
	}
	if _, _, err := signingCommand(ctx, "/usr/bin/codesign", "--verify", "--deep", "--strict", path); err != nil {
		return result, fmt.Errorf("app signature is broken: %w", err)
	}
	out, diagnostic, err := signingCommand(ctx, "/usr/bin/codesign", "--display", "--verbose=4", "--all-architectures", path)
	if err != nil {
		return result, err
	}
	text := string(out) + "\n" + string(diagnostic)
	flags := codeFlags.FindAllStringSubmatch(text, -1)
	if len(flags) == 0 {
		return result, errors.New("cannot read app signing flags")
	}
	for i, f := range flags {
		n, err := strconv.ParseUint(f[1], 16, 32)
		// Only ad-hoc and linker provenance are supported. In particular, do
		// not accidentally drop hardened runtime or restrictive behavior flags.
		if err != nil || n & ^uint64(0x2|0x20000) != 0 {
			return result, errors.New("local signing does not support this app's signature options")
		}
		adhoc := n&0x2 != 0
		if i > 0 && result.Adhoc != adhoc {
			return result, errors.New("inconsistent architecture signing flags")
		}
		result.Adhoc = adhoc
	}
	result.Identifier = id
	identifiers, teams := 0, 0
	for _, l := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(l, "Identifier="):
			identifiers++
			if strings.TrimPrefix(l, "Identifier=") != id {
				return result, errors.New("signature and Info.plist identifiers differ")
			}
		case strings.HasPrefix(l, "TeamIdentifier="):
			team := strings.TrimPrefix(l, "TeamIdentifier=")
			if team == "not set" {
				team = ""
			}
			if teams > 0 && result.Team != team {
				return result, errors.New("inconsistent architecture signing teams")
			}
			teams++
			result.Team = team
		case strings.HasPrefix(l, "CDHash="):
			h := strings.TrimPrefix(l, "CDHash=")
			if b, err := hex.DecodeString(h); err != nil || len(b) != 20 {
				return result, errors.New("invalid app CDHash")
			}
			result.Hashes = append(result.Hashes, h)
		case strings.HasPrefix(l, "Runtime Version=") || strings.Contains(l, "Constraint"):
			return result, errors.New("local signing does not support runtime or launch constraints")
		}
	}
	if identifiers != len(flags) || teams != len(flags) || len(result.Hashes) != len(flags) {
		return result, errors.New("incomplete app signature metadata")
	}
	slices.Sort(result.Hashes)
	out, _, err = signingCommand(ctx, "/usr/bin/codesign", "--display", "--entitlements", "-", "--xml", path)
	if err != nil {
		return result, err
	}
	if len(bytes.TrimSpace(out)) != 0 {
		return result, errors.New("local signing does not support app entitlements")
	}
	out, diagnostic, err = signingCommand(ctx, "/usr/bin/codesign", "--display", "--requirements", "-", path)
	if err != nil {
		return result, err
	}
	for _, l := range strings.Split(string(out)+"\n"+string(diagnostic), "\n") {
		l = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "#"))
		if r, ok := strings.CutPrefix(l, "designated => "); ok {
			if result.Requirement != "" && result.Requirement != r {
				return result, errors.New("inconsistent designated requirements")
			}
			result.Requirement = r
		}
	}
	if result.Requirement == "" {
		return result, errors.New("cannot read app designated requirement")
	}
	if !result.Adhoc {
		if err := embeddedSigningCertificate(ctx, path, &result); err != nil {
			return result, err
		}
	} else if result.Team != "" || !strings.Contains(text, "Signature=adhoc") {
		return result, errors.New("app is not explicitly ad-hoc signed")
	}
	return result, nil
}

func embeddedSigningCertificate(ctx context.Context, path string, s *signingSnapshot) error {
	dir, err := os.MkdirTemp("", "magpie-signing-audit-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	prefix := filepath.Join(dir, "cert")
	if _, _, err := signingCommand(ctx, "/usr/bin/codesign", "--display", "--extract-certificates", prefix, path); err != nil {
		return err
	}
	b, err := os.ReadFile(prefix + "0")
	if err != nil {
		return err
	}
	cert, err := x509.ParseCertificate(b)
	if err != nil {
		return err
	}
	if err := signingCertificate(cert); err != nil {
		return err
	}
	if s.Team != cert.Subject.OrganizationalUnit[0] {
		return errors.New("embedded certificate and signature teams differ")
	}
	s.Fingerprint = certFingerprint(cert)
	return nil
}

func signingRequirement(ctx context.Context, app, requirement string) error {
	_, _, err := signingCommand(ctx, "/usr/bin/codesign", "--verify", "--deep", "--strict", "-R", "="+requirement, app)
	return err
}

func verifyLocalPair(ctx context.Context, c *SigningConfig, bundle, app string) (signingSnapshot, signingSnapshot, error) {
	installed, err := inspectSigningApp(ctx, bundle)
	if err != nil {
		return installed, signingSnapshot{}, err
	}
	if err := authorizeInstalled(c, installed); err != nil {
		return installed, signingSnapshot{}, err
	}
	payload, err := inspectSigningApp(ctx, app)
	if err != nil {
		return installed, payload, err
	}
	if err := matchesSigningPin(c, payload); err != nil {
		return installed, payload, err
	}
	if !installed.Adhoc {
		if installed.Requirement != payload.Requirement {
			return installed, payload, errors.New("the signed update changes the installed designated requirement")
		}
		if err := signingRequirement(ctx, app, installed.Requirement); err != nil {
			return installed, payload, err
		}
	}
	return installed, payload, nil
}

func signLocalApp(ctx context.Context, c *SigningConfig, bundle, app string) (signingSnapshot, signingSnapshot, error) {
	installed, err := inspectSigningApp(ctx, bundle)
	if err != nil {
		return installed, signingSnapshot{}, err
	}
	if err := authorizeInstalled(c, installed); err != nil {
		return installed, signingSnapshot{}, err
	}
	original, err := inspectSigningApp(ctx, app)
	if err != nil {
		return installed, original, err
	}
	if original.Identifier != c.BundleID {
		return installed, original, errors.New("the downloaded app has a different bundle identifier")
	}
	if !original.Adhoc {
		return verifyLocalPair(ctx, c, bundle, app)
	}
	id, err := findSigningIdentity(ctx, c.Fingerprint)
	if err != nil {
		return installed, original, err
	}
	if id.Team != c.Team {
		return installed, original, errors.New("the pinned certificate team changed")
	}
	if _, _, err := signingCommand(ctx, "/usr/bin/codesign", "--force", "--sign", c.Fingerprint, "--identifier", c.BundleID, "--options", "0", "--timestamp=none", app); err != nil {
		return installed, original, fmt.Errorf("local app signing failed: %w", err)
	}
	return verifyLocalPair(ctx, c, bundle, app)
}
