package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/proc"
)

func TestUpdateStaleCLIHintPreservesSignedApp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the macOS hint uses a POSIX shell")
	}
	for _, name := range []string{"plain", "path with spaces", "developer's app $unchanged"} {
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), name)
			app := filepath.Join(root, "magpie.app")
			bin := filepath.Join(app, "Contents", "MacOS", "magpie")
			stale := filepath.Join(root, "cli", "magpie")
			policy := filepath.Join(root, "update-signing.json")
			for _, dir := range []string{filepath.Dir(bin), filepath.Dir(stale)} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			for name, body := range map[string]string{bin: "locally signed app", stale: "old cli copy", policy: "retained signing pin"} {
				if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			hint := staleCLIHint(app, stale)
			if strings.Contains(strings.ToLower(hint), "install") {
				t.Fatalf("stale CLI advice can reinstall the signed app: %s", hint)
			}
			_, command, ok := strings.Cut(hint, "; link it: ")
			if !ok {
				t.Fatalf("stale CLI advice has no link command: %s", hint)
			}
			if output, err := proc.Command("sh", "-c", command).CombinedOutput(); err != nil {
				t.Fatalf("link advice failed: %v: %s", err, output)
			}
			if target, err := os.Readlink(stale); err != nil || target != bin {
				t.Fatalf("CLI link = %q, %v; want %q", target, err, bin)
			}
			for name, want := range map[string]string{bin: "locally signed app", stale: "locally signed app", policy: "retained signing pin"} {
				if body, err := os.ReadFile(name); err != nil || string(body) != want {
					t.Fatalf("%s = %q, %v; want %q", name, body, err, want)
				}
			}
		})
	}
}
