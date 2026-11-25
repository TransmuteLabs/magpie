package netproxy

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/settings"
)

// The settings status must not display credentials; requests still need them.
func TestDescribeHidesProxyCredentials(t *testing.T) {
	for _, source := range []string{"settings", "environment", "system"} {
		for _, tc := range []struct{ name, raw, want string }{
			{"plain", "socks5://127.0.0.1:1080", "socks5://127.0.0.1:1080"},
			{"authenticated", "socks5://alice:secret@127.0.0.1:1080", "socks5://127.0.0.1:1080"},
			{"encoded", "socks5h://ali%40ce:p%3Aa%2Fss%25@[::1]:1080", "socks5h://[::1]:1080"},
			{"username", "http://alice@127.0.0.1:7890", "http://127.0.0.1:7890"},
		} {
			t.Run(source+"/"+tc.name, func(t *testing.T) {
				t.Setenv("XDG_CONFIG_HOME", t.TempDir())
				for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"} {
					t.Setenv(k, "")
				}
				sysCache.at, sysCache.p = farFuture, Proxy{}
				t.Cleanup(func() { sysCache.at = zero })
				switch source {
				case "settings":
					if err := settings.Save(settings.Settings{Proxy: tc.raw}); err != nil {
						t.Fatal(err)
					}
				case "environment":
					t.Setenv("ALL_PROXY", tc.raw)
				case "system":
					sysCache.p.URL = tc.raw
				}
				if got, from := Describe(); got != tc.want || from != source {
					t.Fatalf("description = %q (%s), want %q (%s)", got, from, tc.want, source)
				}
				u, _ := url.Parse("https://vendor.example/request")
				if p, err := For(u); err != nil || p == nil || p.String() != tc.raw {
					t.Fatalf("description changed the proxy used by requests: %v", err)
				}
			})
		}
	}
}

func TestDescribeHidesMalformedSystemProxy(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(k, "")
	}
	sysCache.at, sysCache.p = farFuture, Proxy{URL: "socks5://alice:secret%zz@127.0.0.1:1080"}
	t.Cleanup(func() { sysCache.at = zero })
	if got, from := Describe(); got != "" || from != "system" {
		t.Fatalf("malformed proxy description = %q (%s)", got, from)
	}
}

// #1886: Redact is what every log line, error, status and GUI text shows
// of a proxy. The user and password are written where a user writes them:
// http and socks5, with and without a scheme, percent-encoded, a user
// without a password. None of it is shown; the rest is kept as written.
func TestRedactLeavesOutUserAndPassword(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"http://alice:s3cret@10.0.0.2:8080", "http://10.0.0.2:8080"},
		{"https://alice:s3cret@proxy.example:443", "https://proxy.example:443"},
		{"alice:s3cret@10.0.0.2:8080", "10.0.0.2:8080"},
		{"socks5://alice:s3cret@127.0.0.1:1080", "socks5://127.0.0.1:1080"},
		{"socks5h://alice:s3cret@127.0.0.1:1080", "socks5h://127.0.0.1:1080"},
		{"http://ali%40ce:s3%3Acr%2Fet%25@10.0.0.2:8080", "http://10.0.0.2:8080"},
		{"socks5://ali%40ce:s3%40cret@[::1]:1080", "socks5://[::1]:1080"},
		{"ali%40ce:s3%40cret@10.0.0.2:8080", "10.0.0.2:8080"},
		{"http://alice@10.0.0.2:8080", "http://10.0.0.2:8080"},
		{"socks5://alice@127.0.0.1:1080", "socks5://127.0.0.1:1080"},
		{"alice@10.0.0.2:8080", "10.0.0.2:8080"},
		{"http://alice:@10.0.0.2:8080", "http://10.0.0.2:8080"},
		{"socks5://alice:s3cret%zz@127.0.0.1:1080", ""},
		{"alice:s3cret%zz@10.0.0.2:8080", ""},
		{"http://10.0.0.2:8080", "http://10.0.0.2:8080"},
		{"10.0.0.2:8080", "10.0.0.2:8080"},
		{"direct", "direct"},
		{"", ""},
	} {
		got := Redact(tc.raw)
		if got != tc.want {
			t.Errorf("Redact(%q) = %q, want %q", tc.raw, got, tc.want)
		}
		for _, secret := range []string{"ali", "s3"} {
			if strings.Contains(got, secret) {
				t.Errorf("Redact(%q) = %q shows %q", tc.raw, got, secret)
			}
		}
	}
}

func TestDescribeRespectsEnvNoProxyBypass(t *testing.T) {
	const childEnv = "MAGPIE_TEST_DESCRIBE_ENV_BYPASS"
	const fallback = "http://127.0.0.1:7892"
	if os.Getenv(childEnv) != "" {
		clearProxyEnv(t)
		t.Setenv("HTTPS_PROXY", "http://127.0.0.1:7890")
		t.Setenv("NO_PROXY", "chatgpt.com")
		sysCache.p = Proxy{URL: fallback}
		if got, source := Describe(); got != "" || source != "none" {
			t.Fatalf("Describe() with HTTPS_PROXY + NO_PROXY = %q (%s), want \"\" (none)", got, source)
		}
		target, _ := url.Parse("https://chatgpt.com")
		if proxy, err := For(target); err != nil || proxy != nil {
			t.Fatalf("For(chatgpt.com) = %v, %v, want nil, nil", proxy, err)
		}
		return
	}

	for _, fallbackKind := range []string{"system", "shell"} {
		t.Run("all_proxy/"+fallbackKind, func(t *testing.T) {
			clearProxyEnv(t)
			if fallbackKind == "system" {
				sysCache.p = Proxy{URL: fallback}
			} else {
				shellSays(t, map[string]string{"HTTPS_PROXY": fallback})
			}
			t.Setenv("ALL_PROXY", "http://127.0.0.1:7890")
			t.Setenv("NO_PROXY", "chatgpt.com")
			if got, source := Describe(); got != "" || source != "none" {
				t.Fatalf("Describe() with ALL_PROXY + NO_PROXY = %q (%s), want \"\" (none)", got, source)
			}
			t.Setenv("ALL_PROXY", "")
			wantSource := "system"
			if fallbackKind == "shell" {
				wantSource = "environment"
			}
			if got, source := Describe(); got != fallback || source != wantSource {
				t.Fatalf("Describe() after clearing ALL_PROXY = %q (%s), want %q (%s)", got, source, fallback, wantSource)
			}
		})
	}

	t.Run("https_proxy/system", func(t *testing.T) {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestDescribeRespectsEnvNoProxyBypass$")
		cmd.Env = append(os.Environ(), childEnv+"=1", "GORACE=atexit_sleep_ms=0")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("isolated Describe() HTTPS_PROXY bypass check failed: %v\n%s", err, output)
		}
	})
}
