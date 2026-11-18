package protocolext

import (
	"context"
	"net/http"
	"testing"

	sdk "github.com/yetone/magpie/pkg/protocolext"
)

type adapter struct{}

func (adapter) Contract() sdk.Contract { return sdk.InboundContract() }
func (adapter) ID() string             { return "dummy" }
func (adapter) Path() string           { return "/dummy/rpc" }
func (adapter) Decode(context.Context, http.Header, []byte) (sdk.Request, error) {
	return sdk.Request{}, nil
}
func (adapter) Reply(http.ResponseWriter) sdk.Response { return nil }

func TestHostRequiresRouteOwnershipAndKeepsExactRegistry(t *testing.T) {
	registry, err := sdk.New(adapter{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(registry, nil); err == nil {
		t.Fatal("host accepted registration without an ownership check")
	}
	if _, err := New(registry, func(string) bool { return true }); err == nil {
		t.Fatal("host accepted a reserved route")
	}
	host, err := New(registry, func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if !host.ClientPath("/dummy/rpc") || host.ClientPath("/dummy/rpc/extra") {
		t.Fatal("host did not preserve exact route ownership")
	}
	empty, err := New(nil, func(string) bool { return false })
	if err != nil || empty.ClientPath("/dummy/rpc") {
		t.Fatal("empty host acquired a route", err)
	}
	var absent *Host
	if absent.ClientPath("/dummy/rpc") {
		t.Fatal("nil host acquired a route")
	}
}
