package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

const releaseAuthTestToken = "synthetic_github_release_token"

type releaseAuthRequest struct {
	location string
	auth     string
	accept   string
}

type releaseAuthFixture struct {
	repository string
	fixture    *releaseFixture
	challenge  int
	requests   []releaseAuthRequest
	override   func(*http.Request) *http.Response
}

func newReleaseAuthFixture(t *testing.T, repository string) *releaseAuthFixture {
	t.Helper()
	fixture := releaseNewFixture(t, releasePlatformName(), releaseTestPackage(t, "exec", "1.2.3"))
	fixture.assets[0].ID, fixture.assets[1].ID = 101, 202
	fixture.assets[0].URL = "https://github.com/not-the-owner/not-the-repo/releases/download/wrong/archive.zip"
	fixture.assets[1].URL = "https://cdn.example.invalid/not-the-checksums"
	return &releaseAuthFixture{repository: repository, fixture: fixture, challenge: http.StatusNotFound}
}

func (fixture *releaseAuthFixture) base() string {
	return "https://api.github.com/repos/" + fixture.repository + "/releases/"
}

func (fixture *releaseAuthFixture) client(t *testing.T) *http.Client {
	t.Helper()
	return &http.Client{Transport: releaseHTTPFunc(func(request *http.Request) (*http.Response, error) {
		fixture.requests = append(fixture.requests, releaseAuthRequest{
			location: request.URL.String(), auth: request.Header.Get("Authorization"), accept: request.Header.Get("Accept"),
		})
		if request.Header.Get("Cookie") != "" || request.Header.Get("Proxy-Authorization") != "" {
			t.Error("release request carried ambient credentials")
		}
		if fixture.override != nil {
			if response := fixture.override(request); response != nil {
				return response, nil
			}
		}
		switch request.URL.String() {
		case fixture.base() + "latest", fixture.base() + "tags/v1.2.3":
			if request.Header.Get("Authorization") == "" && fixture.challenge != 0 {
				return releaseTestResponse(request, fixture.challenge, []byte(`{"message":"Not Found"}`)), nil
			}
			assets := make([]map[string]any, 0, len(fixture.fixture.assets))
			for _, asset := range fixture.fixture.assets {
				assets = append(assets, map[string]any{
					"id": asset.ID, "name": asset.Name, "size": asset.Size,
					"browser_download_url": asset.URL,
					"url":                  "https://api.github.com/repos/other/repository/releases/assets/999",
				})
			}
			body, err := json.Marshal(map[string]any{"tag_name": fixture.fixture.tag, "assets": assets})
			if err != nil {
				t.Fatal(err)
			}
			return releaseTestResponse(request, http.StatusOK, body), nil
		case fixture.base() + "assets/101":
			return releaseTestResponse(request, http.StatusOK, fixture.fixture.archive), nil
		case fixture.base() + "assets/202":
			return releaseTestResponse(request, http.StatusOK, fixture.fixture.checksums), nil
		}
		return nil, errors.New("offline fixture rejected an unregistered request")
	})}
}

func TestReleaseAuthPublicDoesNotResolveCredentials(t *testing.T) {
	for _, ids := range []bool{false, true} {
		t.Run(map[bool]string{false: "browser assets", true: "API assets"}[ids], func(t *testing.T) {
			root := releasePrivateTemps(t)
			lookups := 0
			resolver := func(context.Context, string) (string, error) {
				lookups++
				return releaseAuthTestToken, nil
			}
			var client *http.Client
			var requests *[]releaseAuthRequest
			if ids {
				fixture := newReleaseAuthFixture(t, "owner/repo")
				fixture.challenge = 0
				client, requests = fixture.client(t), &fixture.requests
			} else {
				fixture := releaseNewFixture(t, releasePlatformName(), releaseTestPackage(t, "exec", "1.2.3"))
				client = fixture.client(t)
			}
			prepared, cleanup, err := prepareRelease(t.Context(), client, "github:owner/repo", resolver)
			if err != nil {
				t.Fatal(err)
			}
			if prepared.Directory == "" || lookups != 0 {
				t.Fatalf("public release resolved credentials %d times", lookups)
			}
			if requests != nil {
				for _, request := range *requests {
					if request.auth != "" {
						t.Fatal("public API asset carried a credential")
					}
				}
			}
			cleanup()
			releaseAssertEmpty(t, root)
		})
	}
}

func TestReleaseAuthPrivateMetadataAndCanonicalAssets(t *testing.T) {
	for _, status := range []int{401, 403, 404} {
		for _, source := range []string{"github:owner/repo", "github:owner/repo@v1.2.3"} {
			t.Run(http.StatusText(status)+"/"+source, func(t *testing.T) {
				root := releasePrivateTemps(t)
				fixture := newReleaseAuthFixture(t, "owner/repo")
				fixture.challenge = status
				lookups := 0
				prepared, cleanup, err := prepareRelease(t.Context(), fixture.client(t), source,
					func(ctx context.Context, repository string) (string, error) {
						lookups++
						if repository != "owner/repo" {
							t.Fatalf("credential lookup escaped the source: %q", repository)
						}
						return releaseAuthTestToken, nil
					})
				if err != nil {
					t.Fatal(err)
				}
				if lookups != 1 || len(fixture.requests) != 4 || fixture.requests[0].auth != "" ||
					fixture.requests[1].auth != "Bearer "+releaseAuthTestToken {
					t.Fatalf("private lookup/retry = %d, %+v", lookups, fixture.requests)
				}
				for _, request := range fixture.requests[2:] {
					if !strings.HasPrefix(request.location, fixture.base()+"assets/") || request.auth != "Bearer "+releaseAuthTestToken ||
						request.accept != "application/octet-stream" {
						t.Fatalf("private asset did not use its canonical API identity: %+v", request)
					}
				}
				provenance, err := json.Marshal(prepared.Source)
				if err != nil || bytes.Contains(provenance, []byte(releaseAuthTestToken)) {
					t.Fatalf("credential entered provenance: %s, %v", provenance, err)
				}
				cleanup()
				releaseAssertEmpty(t, root)
			})
		}
	}
}

func TestReleaseAuthRequiresIDsAndRejectsExpiredCredentials(t *testing.T) {
	for _, scenario := range []string{"no credential", "helper failure", "bad token", "metadata expired", "checksums expired", "archive expired", "archive ID missing", "checksums ID missing"} {
		t.Run(scenario, func(t *testing.T) {
			root := releasePrivateTemps(t)
			fixture := newReleaseAuthFixture(t, "owner/repo")
			switch scenario {
			case "archive ID missing":
				fixture.fixture.assets[0].ID = 0
			case "checksums ID missing":
				fixture.fixture.assets[1].ID = 0
			}
			fixture.override = func(request *http.Request) *http.Response {
				expired := scenario == "metadata expired" && request.URL.String() == fixture.base()+"latest" ||
					scenario == "checksums expired" && request.URL.String() == fixture.base()+"assets/202" ||
					scenario == "archive expired" && request.URL.String() == fixture.base()+"assets/101"
				if expired && request.Header.Get("Authorization") != "" {
					return releaseTestResponse(request, 401, []byte(releaseAuthTestToken))
				}
				return nil
			}
			lookups := 0
			prepared, cleanup, err := prepareRelease(t.Context(), fixture.client(t), "github:owner/repo",
				func(context.Context, string) (string, error) {
					lookups++
					switch scenario {
					case "no credential":
						return "", nil
					case "helper failure":
						return "", errors.New("helper stderr contains " + releaseAuthTestToken)
					case "bad token":
						return releaseAuthTestToken + "\nAuthorization: injected", nil
					}
					return releaseAuthTestToken, nil
				})
			if err == nil || cleanup != nil || prepared.Directory != "" || lookups != 1 {
				if cleanup != nil {
					cleanup()
				}
				t.Fatalf("invalid private release prepared: %+v, %v; lookups=%d", prepared, err, lookups)
			}
			if strings.Contains(err.Error(), releaseAuthTestToken) {
				t.Fatal("private release error exposed its credential")
			}
			if strings.Contains(scenario, "ID missing") {
				if !strings.Contains(err.Error(), "asset IDs") || len(fixture.requests) != 2 {
					t.Fatalf("private metadata fell back to browser assets: %v, %+v", err, fixture.requests)
				}
			} else if !strings.Contains(err.Error(), "gh auth login") || !strings.Contains(err.Error(), "SSH-only") {
				t.Fatalf("missing helpful authentication guidance: %v", err)
			}
			releaseAssertEmpty(t, root)
		})
	}
}

func TestReleaseAuthStripsEveryRedirectAndNeverReattaches(t *testing.T) {
	for _, target := range []string{
		"https://cdn.example.invalid/checksums",
		"https://github.com/owner/repo/releases/download/v1.2.3/checksums.txt",
		"https://api.github.com/repos/owner/repo/releases/assets/303",
		"https://api.github.com/repos/other/repo/releases/assets/303",
		"https://api.github.com:443/repos/owner/repo/releases/assets/303",
	} {
		t.Run(target, func(t *testing.T) {
			fixture := newReleaseAuthFixture(t, "owner/repo")
			fixture.override = func(request *http.Request) *http.Response {
				if request.URL.String() == fixture.base()+"assets/202" {
					response := releaseTestResponse(request, 302, nil)
					response.Header.Set("Location", target)
					return response
				}
				if request.URL.String() == target {
					if request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" ||
						request.Header.Get("Proxy-Authorization") != "" {
						t.Fatal("redirected release request leaked a credential")
					}
					return releaseTestResponse(request, 200, fixture.fixture.checksums)
				}
				return nil
			}
			client := fixture.client(t)
			redirects := 0
			client.CheckRedirect = func(request *http.Request, previous []*http.Request) error {
				redirects++
				if request.Header.Get("Authorization") != "" {
					t.Fatal("redirect callback received a release credential")
				}
				request.Header.Set("Authorization", "must-be-stripped")
				request.Header.Set("Cookie", "must-be-stripped")
				request.Header.Set("Proxy-Authorization", "must-be-stripped")
				return nil
			}
			prepared, cleanup, err := prepareRelease(t.Context(), client, "github:owner/repo",
				func(context.Context, string) (string, error) { return releaseAuthTestToken, nil })
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			if prepared.Directory == "" || redirects != 1 {
				t.Fatalf("redirect did not complete anonymously: %d, %v", redirects, err)
			}
		})
	}
}

func TestReleaseAuthDoesNotResolveRedirectedOrUnrelatedFailures(t *testing.T) {
	for _, scenario := range []string{"redirected 404", "redirected original 404", "500", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newReleaseAuthFixture(t, "owner/repo")
			calls := 0
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			fixture.override = func(request *http.Request) *http.Response {
				calls++
				switch scenario {
				case "500":
					return releaseTestResponse(request, 500, nil)
				case "canceled":
					cancel()
					return releaseTestResponse(request, 401, nil)
				default:
					if calls == 1 {
						response := releaseTestResponse(request, 302, nil)
						target := fixture.base() + "redirected"
						if scenario == "redirected original 404" {
							target = fixture.base() + "latest"
						}
						response.Header.Set("Location", target)
						return response
					}
					return releaseTestResponse(request, 404, nil)
				}
			}
			lookups := 0
			_, cleanup, err := prepareRelease(ctx, fixture.client(t), "github:owner/repo",
				func(context.Context, string) (string, error) {
					lookups++
					return releaseAuthTestToken, nil
				})
			if err == nil || cleanup != nil || lookups != 0 {
				t.Fatalf("unrelated failure caused credential discovery: %v, lookups=%d", err, lookups)
			}
		})
	}
}

func TestReleaseAuthTargetIsExactAndRepositoryScoped(t *testing.T) {
	for _, target := range []string{
		"https://api.github.com/repos/owner/repo/releases/latest",
		"https://api.github.com/repos/owner/repo/releases/assets/123",
	} {
		if !releaseAuthTarget(target, "owner/repo") {
			t.Fatalf("canonical API target refused: %s", target)
		}
	}
	for _, target := range []string{
		"http://api.github.com/repos/owner/repo/releases/latest",
		"https://api.github.com:443/repos/owner/repo/releases/latest",
		"https://API.GITHUB.COM/repos/owner/repo/releases/latest",
		"https://api.github.com.evil/repos/owner/repo/releases/latest",
		"https://user@api.github.com/repos/owner/repo/releases/latest",
		"https://api.github.com/repos/owner/another/releases/latest",
		"https://api.github.com/repos/owner/repo/releases-other/latest",
		"https://api.github.com/repos/owner/repo/releases/../contents/secret",
		"https://api.github.com/repos/owner/repo/releases/%2e%2e/contents",
		"https://api.github.com/repos/owner/repo/releases/latest?redirect=elsewhere",
		"https://github.com/owner/repo/releases/latest",
		"https://cdn.example.invalid/repos/owner/repo/releases/latest",
	} {
		if releaseAuthTarget(target, "owner/repo") {
			t.Fatalf("unsafe token target accepted: %s", target)
		}
		calls := 0
		client := &http.Client{Transport: releaseHTTPFunc(func(request *http.Request) (*http.Response, error) {
			calls++
			return releaseTestResponse(request, 200, nil), nil
		})}
		if _, err := releaseDownloadRequest(t.Context(), client, target, io.Discard, 10, releaseAuthTestToken, "owner/repo", true); err == nil || calls != 0 {
			t.Fatalf("unsafe target reached transport: %s, %v, calls=%d", target, err, calls)
		}
	}
}

func TestReleaseAuthPreparationDoesNotShareCredentialsOrMutateEnvironment(t *testing.T) {
	before := os.Environ()
	resolved := []string{}
	for _, repository := range []string{"owner/first", "owner/second", "owner/first"} {
		fixture := newReleaseAuthFixture(t, repository)
		client := fixture.client(t)
		prepared, cleanup, err := prepareRelease(t.Context(), client, "github:"+repository,
			func(ctx context.Context, got string) (string, error) {
				resolved = append(resolved, got)
				return releaseAuthTestToken, nil
			})
		if err != nil {
			t.Fatal(err)
		}
		if prepared.Source.Reference != "github:"+repository || client.Jar != nil {
			t.Fatalf("preparation changed caller state: %+v", prepared)
		}
		cleanup()
	}
	if !reflect.DeepEqual(resolved, []string{"owner/first", "owner/second", "owner/first"}) ||
		!reflect.DeepEqual(before, os.Environ()) {
		t.Fatal("credential state escaped one release preparation")
	}
}

func TestReleaseAuthTransportErrorsStayRedactedAndUnwrap(t *testing.T) {
	intercepted := errors.New("intercepted transport " + releaseAuthTestToken)
	for _, cause := range []error{intercepted, context.Canceled, context.DeadlineExceeded} {
		client := &http.Client{Transport: releaseHTTPFunc(func(request *http.Request) (*http.Response, error) {
			return nil, cause
		})}
		_, cleanup, err := prepareRelease(t.Context(), client, "github:owner/repo",
			func(context.Context, string) (string, error) {
				t.Fatal("transport failure caused credential discovery")
				return "", nil
			})
		var requestError *url.Error
		if cleanup != nil || !errors.Is(err, cause) || !errors.As(err, &requestError) ||
			strings.Contains(err.Error(), releaseAuthTestToken) || strings.Contains(err.Error(), "https://") {
			t.Fatalf("transport classification or redaction failed: %v", err)
		}
	}
}

func TestReleaseCredentialResolverPrecedenceAndPromptDisabling(t *testing.T) {
	for _, scenario := range []string{"GH_TOKEN", "GITHUB_TOKEN", "gh", "git"} {
		t.Run(scenario, func(t *testing.T) {
			env := map[string]string{}
			if scenario == "GH_TOKEN" {
				env["GH_TOKEN"], env["GITHUB_TOKEN"] = "synthetic-gh-env", "must-not-win"
			}
			if scenario == "GITHUB_TOKEN" {
				env["GITHUB_TOKEN"] = "synthetic-github-env"
			}
			var commands []releaseCredentialCommand
			token, err := resolveReleaseToken(t.Context(), "owner/repo", func(name string) string { return env[name] },
				func(ctx context.Context, command releaseCredentialCommand) ([]byte, error) {
					commands = append(commands, command)
					if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) > releaseCredentialWait {
						t.Fatal("credential command has no bounded deadline")
					}
					if command.Name == "gh" {
						if !reflect.DeepEqual(command.Args, []string{"auth", "token", "--hostname", "github.com"}) ||
							!strings.Contains(strings.Join(command.Env, "\n"), "GH_PROMPT_DISABLED=1") || command.Input != "" {
							t.Fatalf("gh credential command changed: %+v", command)
						}
						if scenario == "gh" {
							return []byte("synthetic-gh-cli\n"), nil
						}
						return nil, errors.New("gh is unavailable")
					}
					want := []string{"-c", "credential.interactive=false", "-c", "core.askPass=", "credential", "fill"}
					if command.Name != "git" || !reflect.DeepEqual(command.Args, want) ||
						command.Input != "protocol=https\nhost=github.com\npath=owner/repo.git\n\n" {
						t.Fatalf("git lookup is not repository-scoped/noninteractive: %+v", command)
					}
					for _, variable := range []string{"GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never", "GIT_ASKPASS=", "SSH_ASKPASS="} {
						if !strings.Contains(strings.Join(command.Env, "\n"), variable) {
							t.Fatalf("credential lookup could prompt: missing %s", variable)
						}
					}
					return []byte("protocol=https\nhost=github.com\npath=owner/repo.git\nusername=developer\npassword=synthetic-git-cli\n\n"), nil
				})
			expected := map[string]string{"GH_TOKEN": "synthetic-gh-env", "GITHUB_TOKEN": "synthetic-github-env", "gh": "synthetic-gh-cli", "git": "synthetic-git-cli"}[scenario]
			if err != nil || token != expected || len(commands) != map[string]int{"GH_TOKEN": 0, "GITHUB_TOKEN": 0, "gh": 1, "git": 2}[scenario] {
				t.Fatalf("resolver precedence failed: token matches=%v err=%v calls=%d", token == expected, err, len(commands))
			}
		})
	}
}

func TestReleaseCredentialResolverBoundsFailuresAndCancellation(t *testing.T) {
	for _, output := range []string{
		"", "token\nsecond-line", strings.Repeat("x", releaseCredentialLimit+1),
	} {
		calls := 0
		_, err := resolveReleaseToken(t.Context(), "owner/repo", func(string) string { return "" },
			func(ctx context.Context, command releaseCredentialCommand) ([]byte, error) {
				calls++
				if command.Name == "gh" {
					return []byte(output), nil
				}
				return nil, errors.New("private stderr " + releaseAuthTestToken)
			})
		if err == nil || calls != 2 || strings.Contains(err.Error(), releaseAuthTestToken) {
			t.Fatalf("unusable helper output accepted or disclosed: %v, calls=%d", err, calls)
		}
	}
	calls := 0
	_, err := resolveReleaseToken(t.Context(), "owner/repo", func(string) string { return "injected\nheader" },
		func(context.Context, releaseCredentialCommand) ([]byte, error) { calls++; return nil, nil })
	if err == nil || calls != 0 {
		t.Fatal("malformed intentional environment token fell through to another identity")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = resolveReleaseToken(ctx, "owner/repo", func(string) string { t.Fatal("canceled resolver read auth environment"); return "" },
		func(context.Context, releaseCredentialCommand) ([]byte, error) {
			t.Fatal("canceled resolver started a helper")
			return nil, nil
		})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("credential lookup lost cancellation: %v", err)
	}
	for _, repository := range []string{"owner/repo\nhost=elsewhere", "../repo", "owner/repo/extra"} {
		_, err := resolveReleaseToken(t.Context(), repository, func(string) string { t.Fatal("invalid repository read credentials"); return "" },
			func(context.Context, releaseCredentialCommand) ([]byte, error) {
				t.Fatal("invalid repository ran a helper")
				return nil, nil
			})
		if err == nil {
			t.Fatal("unsafe credential repository accepted")
		}
	}
}

func TestReleaseCredentialLookupRetainsParentDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
	defer cancel()
	parentDeadline, _ := ctx.Deadline()
	calls := 0
	_, err := resolveReleaseToken(ctx, "owner/repo", func(string) string { return "" },
		func(commandContext context.Context, command releaseCredentialCommand) ([]byte, error) {
			calls++
			if deadline, exists := commandContext.Deadline(); !exists || deadline.After(parentDeadline) {
				t.Fatal("credential command escaped the parent deadline")
			}
			cancel()
			<-commandContext.Done()
			return nil, commandContext.Err()
		})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("credential discovery outlived its parent or retried: %v, calls=%d", err, calls)
	}
}

func TestReleaseCredentialEnvironmentNeverReachesWorkers(t *testing.T) {
	t.Setenv("GH_TOKEN", releaseAuthTestToken)
	t.Setenv("GITHUB_TOKEN", "synthetic_other_github_token")
	for _, entry := range workerEnv(t.TempDir()) {
		if strings.HasPrefix(entry, "GH_TOKEN=") || strings.HasPrefix(entry, "GITHUB_TOKEN=") {
			t.Fatal("installer credentials entered the worker environment")
		}
	}
	fixture := releaseNewFixture(t, releasePlatformName(), releaseTestPackage(t, "exec", "1.2.3"))
	_, cleanup, err := PrepareRelease(t.Context(), fixture.client(t), "github:owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
}

func TestReleaseGitCredentialScopeAndOutputBound(t *testing.T) {
	for _, output := range []string{
		"protocol=http\nhost=github.com\npassword=secret\n",
		"protocol=https\nhost=another.example\npassword=secret\n",
		"protocol=https\nhost=github.com:443\npassword=secret\n",
		"protocol=https\nhost=github.com\npath=other/repo.git\npassword=secret\n",
		"password=first\npassword=second\n",
		"password=secret\nmalformed\n",
	} {
		if token := releaseGitCredential([]byte(output), "owner/repo"); token != "" {
			t.Fatal("Git credential for another scope was accepted")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	output := &releaseCredentialOutput{cancel: cancel}
	if count, err := output.Write(bytes.Repeat([]byte("x"), releaseCredentialLimit)); err != nil || count != releaseCredentialLimit {
		t.Fatal("exact credential output bound was refused")
	}
	if _, err := output.Write([]byte("x")); err == nil || ctx.Err() == nil || output.buffer.Len() != releaseCredentialLimit {
		t.Fatal("credential output was not bounded and canceled")
	}
	base := []string{"PATH=/synthetic", "HOME=/synthetic-home", "GIT_TERMINAL_PROMPT=1", "git_askpass=unsafe"}
	before := append([]string(nil), base...)
	env := releaseCredentialEnv(base, []string{"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS="})
	if !reflect.DeepEqual(base, before) || strings.Contains(strings.Join(env, "\n"), "unsafe") ||
		strings.Contains(strings.Join(env, "\n"), "GIT_TERMINAL_PROMPT=1") {
		t.Fatal("credential helper environment was mutated or remained interactive")
	}
}
