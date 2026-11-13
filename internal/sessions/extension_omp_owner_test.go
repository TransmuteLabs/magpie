package sessions

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/settings"
)

func TestSessionExtensionSharedOMPAndPiStoreKeepsNativeOwners(test *testing.T) {
	for _, mode := range []string{"agent-override", "flat-session-dir"} {
		test.Run(mode, func(test *testing.T) { sessionSharedOMPAndPiOwners(test, mode) })
	}
}

func sessionSharedOMPAndPiOwners(test *testing.T, mode string) {
	test.Helper()
	setup(test)
	root := test.TempDir()
	test.Setenv("PI_CODING_AGENT_DIR", root)
	copyTree(test, "testdata/pi", root)
	copyTree(test, "testdata/omp/agent", root)
	if mode == "flat-session-dir" {
		home, _ := os.UserHomeDir()
		native := filepath.Join(home, ".custom-omp", "agent")
		copyTree(test, root, native)
		sessions := filepath.Join(native, "sessions")
		buckets, err := os.ReadDir(sessions)
		if err != nil {
			test.Fatal(err)
		}
		shared := filepath.Join(sessions, "shared")
		if err := os.MkdirAll(shared, 0o700); err != nil {
			test.Fatal(err)
		}
		for _, bucket := range buckets {
			entries, err := os.ReadDir(filepath.Join(sessions, bucket.Name()))
			if err != nil {
				test.Fatal(err)
			}
			for _, entry := range entries {
				if err := os.Rename(filepath.Join(sessions, bucket.Name(), entry.Name()), filepath.Join(shared, entry.Name())); err != nil {
					test.Fatal(err)
				}
			}
		}
		test.Setenv("PI_CODING_AGENT_DIR", "")
		test.Setenv("PI_CONFIG_DIR", ".custom-omp")
		test.Setenv("PI_CODING_AGENT_SESSION_DIR", shared)
	}
	const piID = "0199aaaa-1111-7222-8333-444455556666"
	Reset()
	pi, ok := Find("pi", piID)
	if !ok || !strings.Contains(pi.Resume, "pi --session "+piID) {
		test.Fatal("genuine native Pi history lost its owner/resume:", pi)
	}
	omp, ok := Find("omp", ompMain)
	if !ok || !strings.Contains(omp.Resume, "omp --resume "+ompMain) || len(omp.Subs) == 0 {
		test.Fatal("native OMP history/subagents lost their owner/resume:", omp)
	}
	if _, ok := Find("pi", ompMain); ok {
		test.Fatal("native OMP history was also exposed as Pi")
	}
	if _, ok := Find("omp", piID); ok {
		test.Fatal("genuine Pi history was also exposed as OMP")
	}
	for _, consent := range []struct {
		pi  bool
		omp bool
	}{{true, true}, {true, false}, {false, true}, {false, false}} {
		if err := settings.SetSessionReads(func(*settings.SessionReads) *settings.SessionReads {
			return &settings.SessionReads{Asked: true, Agents: map[string]bool{"pi": consent.pi, "omp": consent.omp}}
		}); err != nil {
			test.Fatal(err)
		}
		Reset()
		if _, ok := Find("omp", ompMain); ok != consent.omp {
			test.Fatal("native OMP discovery ignored its read consent:", consent)
		}
		if _, ok := Find("pi", piID); ok != consent.pi {
			test.Fatal("native Pi discovery ignored its read consent:", consent)
		}
		if _, ok := Find("pi", ompMain); ok {
			test.Fatal("OMP history leaked through Pi read consent:", consent)
		}
		if _, ok := Find("omp", piID); ok {
			test.Fatal("Pi history leaked through OMP read consent:", consent)
		}
	}
}

func TestSessionExtensionSharedOMPUnknownPrefixKeepsPiOwner(test *testing.T) {
	original, err := os.ReadFile("testdata/omp/agent/sessions/--work-omp--/2026-09-28T08-00-00-000Z_" + ompMain + ".jsonl")
	if err != nil {
		test.Fatal(err)
	}
	for _, mutation := range []string{"unversioned", "wrong-version", "not-fixed", "invalid-json", "invalid-utf8", "unknown-source", "wrong-padding", "outside-store", "unreadable", "symlink"} {
		test.Run(mutation, func(test *testing.T) {
			if mutation == "unreadable" && runtime.GOOS == "windows" {
				test.Skip("POSIX read permissions")
			}
			setup(test)
			root := test.TempDir()
			test.Setenv("PI_CODING_AGENT_DIR", root)
			path := filepath.Join(root, "sessions", "bucket", "time_"+ompMain+".jsonl")
			body := bytes.Clone(original)
			switch mutation {
			case "unversioned":
				body = bytes.Replace(body, []byte(`"v":1`), []byte(`"z":1`), 1)
			case "wrong-version":
				body = bytes.Replace(body, []byte(`"v":1`), []byte(`"v":2`), 1)
			case "not-fixed":
				body = append([]byte(" "), body...)
			case "invalid-json":
				body[1] = '!'
			case "invalid-utf8":
				body[bytes.Index(body, []byte(`"title":"`))+len(`"title":"`)] = 0xff
			case "unknown-source":
				body = bytes.Replace(body, []byte(`"source":"auto"`), []byte(`"source":"test"`), 1)
			case "wrong-padding":
				body[bytes.Index(body, []byte(`"pad":"`))+len(`"pad":"`)] = '?'
			case "outside-store":
				test.Setenv("PI_CODING_AGENT_DIR", "")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				test.Fatal(err)
			}
			if err := os.WriteFile(path, body, 0o600); err != nil {
				test.Fatal(err)
			}
			if mutation == "unreadable" {
				if err := os.Chmod(path, 0); err != nil {
					test.Fatal(err)
				}
				test.Cleanup(func() { os.Chmod(path, 0o600) })
			} else if mutation == "symlink" {
				if err := os.Rename(path, path+".retained"); err != nil {
					test.Fatal(err)
				}
				if err := os.Symlink(path+".retained", path); err != nil {
					test.Skipf("symlinks unavailable: %v", err)
				}
			}
			files := piFilesIn(root, "")
			if len(files) != 1 || files[0].agent != "pi" || files[0].key != "pi:"+ompMain {
				test.Fatal("unknown/unreadable native prefix changed Pi ownership:", files)
			}
			native := ompFilesUnder(nil, map[string]bool{}, filepath.Join(root, "sessions"))
			if mutation == "outside-store" {
				if len(native) != 1 || native[0].agent != "omp" {
					test.Fatal("nonshared native OMP legacy ownership changed:", native)
				}
			} else if len(native) != 0 {
				test.Fatal("ambiguous shared prefix was incorrectly claimed by OMP:", native)
			}
		})
	}
}
