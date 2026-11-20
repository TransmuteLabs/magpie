package netproxy

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// ALL_PROXY, which curl reads and Go doesn't, is magpie's too when neither
// HTTPS_PROXY nor HTTP_PROXY is set: a shell with only
// ALL_PROXY=socks5://… gets magpie update through it (akic404 on Discord),
// and NO_PROXY still keeps hosts off it.
func TestAllProxyFromTheEnvironment(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(k, "")
	}
	sysCache.at, sysCache.p = farFuture, Proxy{}
	t.Cleanup(func() { sysCache.at = zero })

	t.Setenv("all_proxy", "socks5h://127.0.0.1:1080")
	t.Setenv("NO_PROXY", "internal.example, .corp.example:443")
	gh, _ := url.Parse("https://github.com/yetone/magpie-releases/releases/download/v1/magpie")
	if p, err := For(gh); err != nil || p == nil || p.String() != "socks5h://127.0.0.1:1080" {
		t.Fatalf("ALL_PROXY: %v %v", p, err)
	}
	if proxy, source := Describe(); proxy != "socks5h://127.0.0.1:1080" || source != "environment" {
		t.Fatalf("described as %q (%s)", proxy, source)
	}
	for _, raw := range []string{"https://internal.example/x", "https://api.internal.example/x", "https://git.corp.example/x", "http://127.0.0.1:3/x"} {
		u, _ := url.Parse(raw)
		if p, _ := For(u); p != nil {
			t.Errorf("%s went through %v despite NO_PROXY", raw, p)
		}
	}
	t.Setenv("NO_PROXY", "*")
	if p, _ := For(gh); p != nil {
		t.Fatalf("NO_PROXY=* still proxied: %v", p)
	}
}

func TestAllProxyBypassStopsAutomaticFallback(t *testing.T) {
	for _, source := range []string{"system", "shell"} {
		t.Run(source, func(t *testing.T) {
			clearProxyEnv(t)
			t.Cleanup(SetLookup(func(context.Context, string) ([]net.IPAddr, error) { return nil, nil }))
			const processProxy = "http://127.0.0.1:7890"
			const fallback = "http://127.0.0.1:7891"
			if source == "system" {
				sysCache.p = Proxy{URL: fallback}
			} else {
				shellSays(t, map[string]string{"HTTP_PROXY": fallback})
			}
			t.Setenv("ALL_PROXY", processProxy)
			t.Setenv("NO_PROXY", "trace.example.invalid")
			check := func(host, want string) {
				t.Helper()
				request, err := http.NewRequest(http.MethodGet, "http://"+host+"/cdn-cgi/trace", nil)
				if err != nil {
					t.Fatal(err)
				}
				proxy, err := Func(request)
				got := ""
				if proxy != nil {
					got = proxy.String()
				}
				if err != nil || got != want {
					t.Errorf("proxy for %s = %q, %v, want %q", host, got, err, want)
				}
			}
			check("trace.example.invalid", "")
			check("other.example.invalid", processProxy)
			t.Setenv("ALL_PROXY", "")
			check("trace.example.invalid", fallback)
		})
	}
}

func TestSchemeProxyBypassStopsAutomaticFallback(t *testing.T) {
	const child = "MAGPIE_TEST_SCHEME_PROXY"
	if os.Getenv(child) == "" {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		for _, mode := range []string{"schemes", "cgi"} {
			t.Run(mode, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, exe, "-test.run=^TestSchemeProxyBypassStopsAutomaticFallback$")
				cmd.Env = append(os.Environ(), child+"="+mode, "GORACE=atexit_sleep_ms=0")
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("isolated proxy environment: %v\n%s", err, out)
				}
			})
		}
		return
	}

	clearProxyEnv(t)
	t.Cleanup(SetLookup(func(context.Context, string) ([]net.IPAddr, error) { return nil, nil }))
	const httpProxy = "http://127.0.0.1:7890"
	const httpsProxy = "https://127.0.0.1:7891"
	const fallback = "http://127.0.0.1:7892"
	t.Setenv("REQUEST_METHOD", "")
	t.Setenv("HTTP_PROXY", httpProxy)
	t.Setenv("http_proxy", fallback)
	t.Setenv("NO_PROXY", "trace.example.invalid:80,.corp.example.invalid:443,198.51.100.0/24,2001:db8::/32")
	t.Setenv("no_proxy", "*")

	check := func(raw, want string) {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		proxy, err := Func(request)
		got := ""
		if proxy != nil {
			got = proxy.String()
		}
		if err != nil || got != want {
			t.Errorf("proxy for %s = %q, %v, want %q", raw, got, err, want)
		}
	}
	if os.Getenv(child) == "cgi" {
		t.Setenv("REQUEST_METHOD", http.MethodGet)
		t.Setenv("ALL_PROXY", fallback)
		req := &http.Request{URL: &url.URL{Scheme: "http", Host: "trace.example.invalid"}}
		if proxy, err := Func(req); proxy != nil || err == nil || !strings.Contains(err.Error(), "CGI") {
			t.Fatalf("CGI HTTP_PROXY = %v, %v, want refusal", proxy, err)
		}
		check("https://other.example.invalid/", fallback)
		t.Setenv("ALL_PROXY", "")
		sysCache.p = Proxy{URL: fallback}
		check("https://node.corp.example.invalid/", fallback)
		return
	}

	t.Setenv("https_proxy", httpsProxy)
	for _, source := range []string{"system", "shell", "ALL_PROXY"} {
		t.Run(source, func(t *testing.T) {
			sysCache.p = Proxy{}
			switch source {
			case "system":
				sysCache.p = Proxy{URL: fallback}
			case "shell":
				shellSays(t, map[string]string{"HTTP_PROXY": fallback, "HTTPS_PROXY": fallback})
			case "ALL_PROXY":
				t.Setenv("ALL_PROXY", fallback)
			}
			check("http://trace.example.invalid/", "")
			check("http://trace.example.invalid:8080/", httpProxy)
			check("https://node.corp.example.invalid/", "")
			check("https://corp.example.invalid/", httpsProxy)
			check("http://198.51.100.24/", "")
			check("https://[2001:db8::24]/", "")
			check("https://other.example.invalid/", httpsProxy)
		})
	}
}

func TestNoProxyList(t *testing.T) {
	for _, c := range []struct {
		host, list string
		want       bool
	}{
		{"github.com", "", false},
		{"github.com", "github.com", true},
		{"objects.github.com", "github.com", true},
		{"notgithub.com", "github.com", false},
		{"api.example.com", "*.example.com", true},
		{"example.com", ".example.com", true},
		{"example.com", "example.com:443", true},
		{"localhost", "", true},
	} {
		if got := noProxy(c.host, c.list); got != c.want {
			t.Errorf("noProxy(%q, %q) = %v, want %v", c.host, c.list, got, c.want)
		}
	}
}
