package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

func TestInferenceFixturesOwnOnlyPostRequests(test *testing.T) {
	for _, name := range []string{"paced messages", "body cap"} {
		test.Run(name, func(test *testing.T) {
			paced := &pacedMessages{}
			cap := &capped{proto: provider.Chat, most: 128}
			var handler http.Handler = paced
			if name == "body cap" {
				handler = cap
			}
			server := httptest.NewServer(handler)
			defer server.Close()
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				request, err := http.NewRequestWithContext(test.Context(), method, server.URL+"/", nil)
				if err != nil {
					test.Fatal(err)
				}
				response, err := http.DefaultClient.Do(request)
				if err != nil {
					test.Fatal(err)
				}
				response.Body.Close()
			}
			count := func() int {
				if name == "body cap" {
					return len(cap.calls())
				}
				paced.mu.Lock()
				defer paced.mu.Unlock()
				return paced.n
			}
			if got := count(); got != 0 {
				test.Fatalf("unrelated reads consumed %d inference attempts", got)
			}
			response, err := http.Post(server.URL+"/owned", "application/json", strings.NewReader(`{}`))
			if err != nil {
				test.Fatal(err)
			}
			_, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			if readErr != nil || response.StatusCode != http.StatusOK || count() != 1 {
				test.Fatalf("owned inference was lost: status=%d count=%d error=%v", response.StatusCode, count(), readErr)
			}
		})
	}
}
