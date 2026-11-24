package gui

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/yetone/magpie/internal/library"
)

func TestLibrarySkillProbeAccess(test *testing.T) {
	home := sandboxHome(test)
	test.Setenv("TMPDIR", home)
	test.Setenv("TMP", home)
	test.Setenv("TEMP", home)
	mux := http.NewServeMux()
	libraryRoutes(mux, nil)
	probe := func(test *testing.T, dir string, code int, message string, names ...string) {
		test.Helper()
		body, _ := json.Marshal(map[string]string{"source": dir})
		response := do(mux, httptest.NewRequest(http.MethodPost, "/api/library/skills/probe", bytes.NewReader(body)))
		if response.Code != code || response.Header().Get("Content-Type") != "application/json" {
			test.Fatalf("probe %s returned %d %s: %s", dir, response.Code, response.Header().Get("Content-Type"), response.Body)
		}
		var result struct {
			Candidates []library.Candidate `json:"candidates"`
			Error      string              `json:"error"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			test.Fatal(err)
		}
		if result.Error != message || len(result.Candidates) != len(names) || code == http.StatusOK && result.Candidates == nil {
			test.Fatalf("probe %s returned %s, want error %q and candidates %v", dir, response.Body, message, names)
		}
		for index, name := range names {
			if candidate := result.Candidates[index]; candidate.Name != name || candidate.Path != name {
				test.Fatalf("probe %s candidate %d = %+v, want %s", dir, index, candidate, name)
			}
		}
	}
	deny := func(test *testing.T, dir string, system error) {
		test.Helper()
		if !errors.Is(system, fs.ErrPermission) {
			test.Fatalf("fixture %s did not produce an access error: %v", dir, system)
		}
		probe(test, dir, http.StatusForbidden, system.Error())
	}
	chmod := func(test *testing.T, path string, mode fs.FileMode) {
		test.Helper()
		if err := os.Chmod(path, mode); err != nil {
			test.Fatal(err)
		}
	}
	skill := func(test *testing.T, root, name string) string {
		test.Helper()
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			test.Fatal(err)
		}
		file := filepath.Join(dir, "SKILL.md")
		if err := os.WriteFile(file, []byte("---\nname: "+name+"\n---\n"), 0o600); err != nil {
			test.Fatal(err)
		}
		return file
	}

	test.Run("empty_and_missing", func(test *testing.T) {
		root := test.TempDir()
		probe(test, root, http.StatusOK, "")
		missing := filepath.Join(root, "missing")
		probe(test, missing, http.StatusBadRequest, missing+" isn't a folder")
	})
	test.Run("posix", func(test *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			test.Skip("the test needs Unix file modes to deny access")
		}
		test.Run("metadata_and_partial_results", func(test *testing.T) {
			root := test.TempDir()
			file := skill(test, root, "r02-hidden")
			probe(test, root, http.StatusOK, "", "r02-hidden")
			chmod(test, file, 0)
			test.Cleanup(func() { chmod(test, file, 0o600) })
			if _, err := os.ReadDir(filepath.Dir(file)); err != nil {
				test.Fatal(err)
			}
			_, readErr := os.ReadFile(file)
			deny(test, filepath.Dir(file), readErr)
			deny(test, root, readErr)
			skill(test, root, "r02-visible")
			probe(test, root, http.StatusOK, "", "r02-visible")
			chmod(test, file, 0o600)
			probe(test, root, http.StatusOK, "", "r02-hidden", "r02-visible")
		})
		test.Run("directory_read", func(test *testing.T) {
			root := test.TempDir()
			chmod(test, root, 0o100)
			test.Cleanup(func() { chmod(test, root, 0o700) })
			if _, err := os.ReadFile(filepath.Join(root, "SKILL.md")); !errors.Is(err, fs.ErrNotExist) {
				test.Fatalf("fixture needs a searchable folder without SKILL.md: %v", err)
			}
			_, readErr := os.ReadDir(root)
			deny(test, root, readErr)
		})
		test.Run("folder_stat", func(test *testing.T) {
			root := test.TempDir()
			hidden := filepath.Join(root, ".claude")
			selected := filepath.Join(hidden, "skills")
			if err := os.MkdirAll(selected, 0o700); err != nil {
				test.Fatal(err)
			}
			chmod(test, hidden, 0)
			test.Cleanup(func() { chmod(test, hidden, 0o700) })
			_, statErr := os.Stat(selected)
			deny(test, selected, statErr)
			deny(test, root, statErr)
		})
	})
}
