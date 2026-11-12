package gui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/protocolext"
)

type pageProtocolFixture struct {
	decoded *bool
}

func (pageProtocolFixture) Contract() sdk.Contract { return sdk.CatalogContract() }
func (pageProtocolFixture) ID() string             { return "fixture.catalog" }
func (pageProtocolFixture) Path() string           { return "/fixture.catalog/read" }
func (fixture pageProtocolFixture) Decode(requestContext context.Context, _ http.Header, _ []byte) (sdk.Request, error) {
	*fixture.decoded = true
	if access.Caller(requestContext).KeyID == "" {
		return sdk.Request{}, &sdk.Error{Status: http.StatusForbidden, Message: "missing caller"}
	}
	return sdk.Request{Protocol: sdk.Models}, nil
}
func (pageProtocolFixture) Reply(writer http.ResponseWriter) sdk.Response {
	return pageProtocolResponse{ResponseWriter: writer}
}

type pageProtocolResponse struct {
	http.ResponseWriter
}

func (pageProtocolResponse) Finish(error) error { return nil }

func TestOnePortUsesTheServingProtocolRegistry(t *testing.T) {
	sandboxHome(t)
	t.Setenv("MAGPIE_ADDR", "")
	configuration := settings.Load()
	configuration.LAN = true
	if err := settings.Save(configuration); err != nil {
		t.Fatal(err)
	}
	secret, err := access.Update("add-key", access.Change{Name: "Protocol fixture"})
	if err != nil {
		t.Fatal(err)
	}
	decoded := false
	adapter := pageProtocolFixture{decoded: &decoded}
	registry, err := sdk.New(adapter)
	if err != nil {
		t.Fatal(err)
	}
	server, err := gateway.NewWithProtocols(registry)
	if err != nil {
		t.Fatal(err)
	}
	previous := served.Swap(server)
	t.Cleanup(func() { served.Store(previous) })
	pageReached := false
	handler := onePort(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		pageReached = true
		writer.WriteHeader(http.StatusTeapot)
	}))
	for _, credential := range []string{"", secret} {
		decoded = false
		request := httptest.NewRequest(http.MethodPost, adapter.Path(), strings.NewReader("native"))
		request.RemoteAddr = "127.0.0.1:12345"
		request.Host = "localhost"
		if credential != "" {
			request.Header.Set("Authorization", "Bearer "+credential)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if pageReached {
			t.Fatal("registered protocol was handed to the page")
		}
		if credential == "" {
			if response.Code != http.StatusUnauthorized || decoded {
				t.Fatalf("page-port protocol became owner-local: status=%d decoded=%v", response.Code, decoded)
			}
		} else if response.Code != http.StatusOK || !decoded || !strings.Contains(response.Body.String(), `"object":"list"`) {
			t.Fatalf("authenticated protocol failed: status=%d decoded=%v body=%s", response.Code, decoded, response.Body)
		}
	}
	request := httptest.NewRequest(http.MethodPost, adapter.Path()+"/unknown", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusTeapot || !pageReached {
		t.Fatal("unknown protocol subpath bypassed the page")
	}
}
