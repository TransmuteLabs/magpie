package extension

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func TestIndependentModuleReleaseRecordShape(t *testing.T) {
	for _, test := range []struct {
		name string
		row  string
		fail bool
	}{
		{"null", "null", true},
		{"empty", "{}", true},
		{"null tag", `{"tag_name":null}`, true},
		{"blank tag", `{"tag_name":" "}`, true},
		{"scalar", `"unidentifiable"`, true},
		{"wrong tag type", `{"tag_name":123}`, true},
		{"unrelated optional fields", `{"tag_name":"other-v9.0.0","unused":{"kept":true}}`, false},
		{"draft optional fields", `{"tag_name":"module-v99.0.0","draft":true}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newModuleReleaseServer(t)
			fixture.override = func(request *http.Request) (int, []byte, bool) {
				return http.StatusOK, []byte(`[{"tag_name":"module-v2.0.0"},` + test.row + `]`), true
			}
			auth := &releaseAuthorization{client: fixture.client, repository: "owner/mono"}
			release, err := releaseResolve(t.Context(), auth, "https://api.github.com/repos/owner/mono/releases", "", "module")
			if test.fail {
				if err == nil {
					t.Fatal("unidentifiable release row was treated as a complete list")
				}
			} else if err != nil || release.Tag != "module-v2.0.0" {
				t.Fatalf("valid optional metadata was rejected: tag=%q err=%v", release.Tag, err)
			}
		})
	}
}

func TestIndependentModuleReleaseContinuation(t *testing.T) {
	const endpoint = "https://api.github.com/repos/owner/mono/releases"
	for _, test := range []string{
		"short next", "private short next", "short next uppercase", "last page previous",
		"foreign next", "skipped next", "duplicate next", "malformed link", "next beyond bound",
	} {
		t.Run(test, func(t *testing.T) {
			fixture := newModuleReleaseServer(t)
			fixture.private = test == "private short next"
			pages := []string{}
			var pagesMu sync.Mutex
			fixture.override = func(request *http.Request) (int, []byte, bool) {
				page := request.URL.Query().Get("page")
				pagesMu.Lock()
				pages = append(pages, page)
				pagesMu.Unlock()
				tag := "module-v1.0.0"
				if page == "2" {
					tag = "module-v2.0.0"
				}
				body, err := json.Marshal([]githubRelease{{Tag: tag}})
				if err != nil {
					t.Error(err)
				}
				return http.StatusOK, body, true
			}
			transport := fixture.client.Transport
			fixture.client.Transport = releaseHTTPFunc(func(request *http.Request) (*http.Response, error) {
				response, err := transport.RoundTrip(request)
				if err != nil || response.StatusCode != http.StatusOK {
					return response, err
				}
				if request.URL.Query().Get("page") == "2" || test == "last page previous" {
					response.Header.Set("Link", `<`+endpoint+`?per_page=100&page=1>; rel="prev"`)
					return response, nil
				}
				next := `<` + endpoint + `?per_page=100&page=2>; rel="next"`
				switch test {
				case "short next uppercase":
					next = strings.Replace(next, `"next"`, `"NEXT"`, 1)
				case "foreign next":
					next = `<https://api.github.com/repos/other/repo/releases?per_page=100&page=2>; rel="next"`
				case "skipped next":
					next = `<` + endpoint + `?per_page=100&page=3>; rel="next"`
				case "duplicate next":
					next += ", " + next
				case "malformed link":
					next = "unparseable pagination"
				case "next beyond bound":
					next = `<` + endpoint + `?per_page=100&page=11>; rel="next"`
				}
				response.Header.Set("Link", next)
				return response, nil
			})
			lookups := 0
			auth := &releaseAuthorization{
				client: fixture.client, repository: "owner/mono",
				resolver: func(context.Context, string) (string, error) {
					lookups++
					return releaseAuthTestToken, nil
				},
			}
			release, err := releaseResolve(t.Context(), auth, endpoint, "", "module")
			pagesMu.Lock()
			visited := strings.Join(pages, ",")
			pagesMu.Unlock()
			if strings.Contains(test, "short next") {
				if err != nil || release.Tag != "module-v2.0.0" || visited != "1,2" {
					t.Fatalf("short continuing page published a partial maximum: tag=%q pages=%s err=%v", release.Tag, visited, err)
				}
			} else if test == "last page previous" {
				if err != nil || release.Tag != "module-v1.0.0" {
					t.Fatalf("previous-only terminal page failed: %q %v", release.Tag, err)
				}
			} else if err == nil || visited != "1" {
				t.Fatalf("untrusted continuation was followed or ignored: pages=%s err=%v", visited, err)
			}
			wantLookups := 0
			if fixture.private {
				wantLookups = 1
			}
			if lookups != wantLookups {
				t.Fatalf("pagination credential lookups=%d, want %d", lookups, wantLookups)
			}
		})
	}
}
