package gateway

import (
	"net/http/httptest"
	"testing"

	"github.com/yetone/magpie/internal/access"
)

func TestNativeAccountOperationCallerDelegationIsLocalAndUnrestricted(t *testing.T) {
	for _, name := range []string{"local", "remote", "forwarded", "readonly", "restricted", "limited", "concurrent", "issuer"} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "http://127.0.0.1:3425/fixture.Service/Read", nil)
			request.RemoteAddr = "127.0.0.1:40000"
			who := access.Identity{}
			switch name {
			case "remote":
				request.RemoteAddr = "198.51.100.23:40000"
			case "forwarded":
				request.Header.Set("X-Forwarded-For", "198.51.100.23")
			case "readonly":
				who.ReadOnly = true
			case "restricted":
				who.Models = []string{"fixture/*"}
			case "limited":
				who.Limit = &access.Limit{Tokens: 1}
			case "concurrent":
				who.MaxConcurrency = 1
			case "issuer":
				who.Issuer = &access.Identity{ReadOnly: true}
			}
			request = request.WithContext(access.WithIdentity(request.Context(), who))
			err := nativeAccountCaller(request)
			if (name == "local") != (err == nil) {
				t.Fatalf("native account operation caller %s: %v", name, err)
			}
		})
	}
}
