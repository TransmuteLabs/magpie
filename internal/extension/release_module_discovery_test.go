package extension

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestIndependentModuleReleaseDiscovery(t *testing.T) {
	for _, test := range []string{
		"backport after newer", "interleaved modules", "later page maximum", "private pagination",
		"draft and prerelease", "duplicate tag", "semantic aliases", "later page failure",
		"null later page", "malformed later page", "listing bound", "no stable module",
		"nested module identity", "nested prerelease identity", "malformed selected identity",
		"malformed sibling-like identity", "invalid sibling identity", "selected prerelease delimiter",
		"selected build delimiter alias",
	} {
		t.Run(test, func(t *testing.T) {
			fixture := newModuleReleaseServer(t)
			fixture.private = test == "private pagination"
			fixture.publish(t, "v999.0.0", map[string][]byte{
				moduleReleaseName("module_a__one"): moduleReleaseZIP(t, "module_a__one", "2.0.0", "exec"),
			})
			selected := map[string]any{"tag_name": "module_a__one-v2.0.0", "assets": fixture.releases[0].Assets}
			older := map[string]any{"tag_name": "module_a__one-v1.0.0"}
			other := map[string]any{"tag_name": "module-b-v999.0.0"}
			pages := map[int][]map[string]any{1: {older, other, selected}}
			failure := false
			switch test {
			case "nested module identity":
				pages[1] = []map[string]any{selected, {"tag_name": "module_a__one-v1-v999.0.0"}}
			case "nested prerelease identity":
				pages[1] = []map[string]any{selected, {"tag_name": "module_a__one-vnext-v999.0.0-rc.1"}}
			case "malformed selected identity":
				pages[1], failure = []map[string]any{selected, {"tag_name": "module_a__one-vbroken"}}, true
			case "malformed sibling-like identity":
				pages[1], failure = []map[string]any{selected, {"tag_name": "module_a__one-vbad-vbroken"}}, true
			case "invalid sibling identity":
				pages[1], failure = []map[string]any{selected, {"tag_name": "module_a__one-vbad+alias-v9.0.0"}}, true
			case "selected prerelease delimiter":
				pages[1] = []map[string]any{selected, {"tag_name": "module_a__one-v97.0.0-rc-v2.0.0"}}
			case "selected build delimiter alias":
				pages[1], failure = []map[string]any{selected, {"tag_name": "module_a__one-v2.0.0+build-v3.0.0"}}, true
			case "draft and prerelease":
				pages[1] = []map[string]any{
					{"tag_name": "module_a__one-v99.0.0", "draft": true},
					{"tag_name": "module_a__one-v98.0.0", "prerelease": true},
					{"tag_name": "module_a__one-v97.0.0-rc.1"}, selected,
				}
			case "duplicate tag":
				pages[1], failure = []map[string]any{selected, selected}, true
			case "semantic aliases":
				pages[1], failure = []map[string]any{selected, {"tag_name": "module_a__one-v2.0.0+alias"}}, true
			case "no stable module":
				pages[1], failure = []map[string]any{other}, true
			case "later page maximum", "private pagination", "later page failure", "null later page", "malformed later page", "listing bound":
				pages[1] = []map[string]any{older}
				for index := 1; index < 100; index++ {
					pages[1] = append(pages[1], map[string]any{"tag_name": fmt.Sprintf("unrelated-v%d.0.0", index)})
				}
				pages[2] = []map[string]any{other, selected}
				failure = test != "later page maximum" && test != "private pagination"
			}
			fixture.override = func(request *http.Request) (int, []byte, bool) {
				if request.URL.Path != "/repos/owner/mono/releases" {
					return 0, nil, false
				}
				if request.URL.Query().Get("per_page") != "100" {
					t.Error("module discovery changed its page size")
				}
				page, err := strconv.Atoi(request.URL.Query().Get("page"))
				if err != nil || page < 1 || page > 10 {
					t.Error("module discovery exceeded its explicit page bounds")
				}
				if page == 2 {
					switch test {
					case "later page failure":
						return http.StatusServiceUnavailable, nil, true
					case "null later page":
						return http.StatusOK, []byte("null"), true
					case "malformed later page":
						return http.StatusOK, []byte(`{"not":"an array"}`), true
					}
				}
				listing := pages[page]
				if test == "listing bound" {
					listing = make([]map[string]any, 100)
					for index := range listing {
						listing[index] = map[string]any{"tag_name": fmt.Sprintf("unrelated-v%d.%d.0", page, index)}
					}
					if page == 1 {
						listing[0] = selected
					}
				}
				body, err := json.Marshal(listing)
				if err != nil {
					t.Error(err)
				}
				return http.StatusOK, body, true
			}
			lookups := 0
			prepared, cleanup, err := prepareRelease(t.Context(), fixture.client, "github:owner/mono#module_a__one",
				func(_ context.Context, repository string) (string, error) {
					lookups++
					if repository != "owner/mono" {
						t.Error("module selector changed credential repository")
					}
					return releaseAuthTestToken, nil
				})
			if failure {
				if err == nil {
					cleanup()
					t.Fatal("incomplete or ambiguous module listing selected a release")
				}
			} else {
				if err != nil {
					t.Fatal("highest stable module could not be resolved:", err)
				}
				if prepared.Source.Tag != "module_a__one-v2.0.0" {
					t.Fatal("backport, global latest or another module displaced the highest version")
				}
				cleanup()
			}
			expectedLookups := 0
			if fixture.private {
				expectedLookups = 1
			}
			if lookups != expectedLookups {
				t.Fatalf("pagination credential lookups=%d, want %d", lookups, expectedLookups)
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			for _, request := range fixture.requests {
				if strings.HasSuffix(request.location, "/latest") {
					t.Fatal("scoped source queried repository-global latest")
				}
				if failure && strings.Contains(request.location, "/assets/") {
					t.Fatal("failed complete listing downloaded an asset")
				}
			}
		})
	}
}

func TestIndependentModuleReleasePinsAndPlatform(t *testing.T) {
	for _, test := range []string{"published prerelease", "draft pin", "wrong module tag", "wrong manifest version", "unsupported platform"} {
		t.Run(test, func(t *testing.T) {
			fixture := newModuleReleaseServer(t)
			fixture.publish(t, "v999.0.0", map[string][]byte{
				moduleReleaseName("module"): moduleReleaseZIP(t, "module", "2.1.0-rc.1", "exec"),
			})
			release := map[string]any{
				"tag_name": "module-v2.1.0-rc.1", "prerelease": true, "assets": fixture.releases[0].Assets,
			}
			reference := "github:owner/mono#module@module-v2.1.0-rc.1"
			switch test {
			case "draft pin":
				release["draft"] = true
			case "wrong module tag":
				release["tag_name"] = "other-v2.1.0-rc.1"
			case "wrong manifest version":
				release["tag_name"] = "module-v2.0.0"
				reference = "github:owner/mono#module@module-v2.0.0"
			case "unsupported platform":
				assets := append([]releaseAsset(nil), fixture.releases[0].Assets...)
				assets[0].Name = "magpie-plugin_module__unsupported_arch.zip"
				release = map[string]any{"tag_name": "module-v3.0.0", "assets": assets}
				reference = "github:owner/mono#module"
			}
			fixture.override = func(request *http.Request) (int, []byte, bool) {
				var value any = release
				if request.URL.Path == "/repos/owner/mono/releases" {
					value = []any{release, map[string]any{"tag_name": "module-v1.0.0", "assets": fixture.releases[0].Assets}}
				} else if !strings.Contains(request.URL.Path, "/tags/") {
					return 0, nil, false
				}
				body, err := json.Marshal(value)
				if err != nil {
					t.Error(err)
				}
				return http.StatusOK, body, true
			}
			prepared, cleanup, err := PrepareRelease(t.Context(), fixture.client, reference)
			if test == "published prerelease" {
				if err != nil {
					t.Fatal("exact published prerelease pin failed:", err)
				}
				if prepared.Source.Reference != reference {
					t.Fatal("explicit module pin was not retained")
				}
				cleanup()
			} else if err == nil {
				cleanup()
				t.Fatal("invalid pin or unsupported platform was accepted")
			}
			if test == "unsupported platform" {
				fixture.mu.Lock()
				defer fixture.mu.Unlock()
				for _, request := range fixture.requests {
					if strings.Contains(request.location, "/assets/") {
						t.Fatal("unsupported newest module fell back or downloaded an archive")
					}
				}
			}
		})
	}
}

func TestIndependentModuleReleaseAuthPaginationScope(t *testing.T) {
	for _, query := range []string{"per_page=100&page=1", "per_page=100&page=10"} {
		if !releaseAuthTarget("https://api.github.com/repos/owner/mono/releases?"+query, "owner/mono") {
			t.Fatal("canonical bounded module list endpoint rejected")
		}
	}
	for _, target := range []string{
		"https://api.github.com/repos/owner/other/releases?per_page=100&page=1",
		"https://api.github.com/repos/owner/mono/releases?per_page=100&page=0",
		"https://api.github.com/repos/owner/mono/releases?per_page=100&page=11",
		"https://api.github.com/repos/owner/mono/releases?per_page=100&page=01",
		"https://api.github.com/repos/owner/mono/releases?per_page=100&page=1&extra=1",
		"https://api.github.com/repos/owner/mono/releases/latest?per_page=100&page=1",
		"https://api.github.com/repos/owner/mono/releases?per_page=100&page=1#fragment",
	} {
		if releaseAuthTarget(target, "owner/mono") {
			t.Fatal("noncanonical or foreign paginated endpoint admitted")
		}
	}
}

func TestIndependentModuleReleaseLegacyRepositoryBinding(t *testing.T) {
	fixture := newReleaseAuthFixture(t, "owner/releases-extra")
	lookups := 0
	prepared, cleanup, err := prepareRelease(t.Context(), fixture.client(t), "github:owner/releases-extra@v1.2.3",
		func(_ context.Context, repository string) (string, error) {
			lookups++
			if repository != "owner/releases-extra" {
				t.Error("valid legacy repository was truncated:", repository)
			}
			return releaseAuthTestToken, nil
		})
	if err != nil {
		t.Fatal("legacy private source changed:", err)
	}
	cleanup()
	if lookups != 1 || prepared.Source.Reference != "github:owner/releases-extra@v1.2.3" {
		t.Fatal("legacy private source binding changed")
	}
}

func TestIndependentModuleReleaseOverlappingTagsKeepExactIdentity(t *testing.T) {
	versions := map[string]string{"foo": "1.2.3-rc-v2.0.0", "foo-v1.2.3-rc": "2.0.0"}
	for actual, version := range versions {
		for selected := range versions {
			for _, pinned := range []bool{false, true} {
				t.Run(fmt.Sprintf("actual=%s/selected=%s/pinned=%t", actual, selected, pinned), func(t *testing.T) {
					fixture := newModuleReleaseServer(t)
					archive := moduleReleaseZIP(t, actual, version, "exec")
					fixture.publish(t, "v999.0.0", map[string][]byte{
						moduleReleaseName("foo"): archive, moduleReleaseName("foo-v1.2.3-rc"): archive,
					})
					release := githubRelease{Tag: "foo-v1.2.3-rc-v2.0.0", Assets: fixture.assets}
					fixture.override = func(request *http.Request) (int, []byte, bool) {
						var value any
						if request.URL.Path == "/repos/owner/mono/releases" {
							value = []githubRelease{release}
						} else if strings.Contains(request.URL.Path, "/tags/") {
							value = release
						} else {
							return 0, nil, false
						}
						body, err := json.Marshal(value)
						if err != nil {
							t.Error(err)
						}
						return http.StatusOK, body, true
					}
					reference := "github:owner/mono#" + selected
					if pinned {
						reference += "@" + release.Tag
					}
					prepared, cleanup, err := PrepareRelease(t.Context(), fixture.client, reference)
					wantSuccess := actual == selected && (pinned || selected != "foo")
					if !wantSuccess {
						if err == nil {
							cleanup()
							t.Fatal("overlapping tag activated another module or an unpinned prerelease")
						}
						return
					}
					if err != nil {
						t.Fatal("exact module interpretation failed:", err)
					}
					defer cleanup()
					if prepared.Source.Reference != reference || prepared.Source.Tag != release.Tag {
						t.Fatal("overlapping tag changed exact source identity")
					}
				})
			}
		}
	}
}
