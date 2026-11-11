package plugin

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/yetone/magpie/internal/settings"
)

const bundledZCodePackage = "builtin:zcode"
const bundledZCodeVersion = "3.18.0"

// PrepareZCode adopts legacy Core accounts before the intrinsic provider is
// activated. The provider package supplies it without a plugin dependency.
var PrepareZCode func() error

func bundledZCodeInfo() NPM {
	return NPM{Version: bundledZCodeVersion, Description: "Bundled ZCode provider/account protocol", License: "MIT AND Apache-2.0"}
}

// Only the package's root runtime and attribution files are shipped; its
// development fixtures and tests are deliberately not embedded.
//
//go:embed bundled/zcode/*.mjs bundled/zcode/package.json bundled/zcode/LICENSE.* bundled/zcode/NOTICE*
var bundledZCode embed.FS

var bundleIdentity = sync.OnceValue(func() string {
	h := sha256.New()
	entries, err := fs.ReadDir(bundledZCode, "bundled/zcode")
	if err != nil {
		panic(err) // an invalid embedded filesystem is a build defect
	}
	for _, e := range entries {
		b, err := bundledZCode.ReadFile("bundled/zcode/" + e.Name())
		if err != nil {
			panic(err)
		}
		fmt.Fprintf(h, "%d:%s:%d:", len(e.Name()), e.Name(), len(b))
		_, _ = h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
})

// IsBundledZCode identifies the Core-owned provider, not an installed package.
func IsBundledZCode(spec string) bool {
	return spec == bundledZCodePackage
}

func bundledTarget() string {
	return filepath.Join(Dir(), "bundled", "zcode", bundleIdentity())
}

// syncBundleDir flushes a directory entry on platforms that expose directory
// handles for fsync. Windows does not support syncing directory handles.
func syncBundleDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// ensureBundledZCode publishes an immutable content-addressed generation. A
// racing process may publish the same generation; it is accepted only after
// every embedded byte has been checked. No generation is ever removed.
func ensureBundledZCode() error {
	root := filepath.Dir(bundledTarget())
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("creating ZCode bundle directory: %w", err)
	}
	for _, parent := range []string{settings.Dir(), Dir(), filepath.Dir(root)} {
		if err := syncBundleDir(parent); err != nil {
			return fmt.Errorf("syncing ZCode bundle parent: %w", err)
		}
	}
	if _, err := os.Stat(bundledTarget()); err == nil {
		if err := validateBundledZCode(bundledTarget()); err != nil {
			return err
		}
		return syncBundleDir(root)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("reading ZCode bundle: %w", err)
	}
	tmp, err := os.MkdirTemp(root, ".extract-")
	if err != nil {
		return fmt.Errorf("extracting ZCode bundle: %w", err)
	}
	defer os.RemoveAll(tmp)
	entries, err := fs.ReadDir(bundledZCode, "bundled/zcode")
	if err != nil {
		return err
	}
	for _, e := range entries {
		b, err := bundledZCode.ReadFile("bundled/zcode/" + e.Name())
		if err != nil {
			return err
		}
		f, err := os.OpenFile(filepath.Join(tmp, e.Name()), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		_, err = f.Write(b)
		if err == nil {
			err = f.Sync()
		}
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return fmt.Errorf("writing bundled ZCode %s: %w", e.Name(), err)
		}
	}
	if err := syncBundleDir(tmp); err != nil {
		return fmt.Errorf("syncing ZCode bundle: %w", err)
	}
	if err := os.Rename(tmp, bundledTarget()); err != nil {
		if check := validateBundledZCode(bundledTarget()); check != nil {
			return fmt.Errorf("publishing ZCode bundle: %w (existing generation: %v)", err, check)
		}
		return syncBundleDir(root)
	}
	if err := syncBundleDir(root); err != nil {
		return fmt.Errorf("syncing ZCode bundle directory: %w", err)
	}
	return nil
}

func validateBundledZCode(dir string) error {
	entries, err := fs.ReadDir(bundledZCode, "bundled/zcode")
	if err != nil {
		return err
	}
	actual, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("reading bundled ZCode generation: %w", err)
	}
	if len(actual) != len(entries) {
		return fmt.Errorf("bundled ZCode generation has missing or unexpected files")
	}
	for i, e := range entries {
		if actual[i].Name() != e.Name() || !actual[i].Type().IsRegular() {
			return fmt.Errorf("bundled ZCode generation has a changed file: %s", e.Name())
		}
		want, err := bundledZCode.ReadFile("bundled/zcode/" + e.Name())
		if err != nil {
			return err
		}
		got, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil || !bytes.Equal(got, want) {
			return fmt.Errorf("bundled ZCode generation %s is missing or changed: %s", strings.TrimPrefix(dir, filepath.Dir(dir)+string(filepath.Separator)), e.Name())
		}
	}
	return nil
}
