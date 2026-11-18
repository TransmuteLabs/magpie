package protocolext

import (
	"context"
	"net/http"
	"testing"
)

type bindingCodec struct {
	fixture
	seen context.Context
}

func (codec *bindingCodec) Bind(ctx context.Context) Inbound {
	codec.seen = ctx
	return incompatible{fixture{"changed", "/changed"}}
}

func TestBindDelegatesWithoutChangingFrozenRegistration(t *testing.T) {
	codec := &bindingCodec{fixture: fixture{"codec", "/rpc"}}
	registry, err := New(codec)
	if err != nil {
		t.Fatal(err)
	}
	entry, _ := registry.Lookup("/rpc")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bound := Bind(entry, ctx)
	if codec.seen != ctx || bound.ID() != "codec" || bound.Path() != "/rpc" || bound.Contract().Version.Major != 0 {
		t.Fatal("binding lost its context or frozen authority")
	}
	exposed := bound.Contract()
	exposed.Capabilities[0] = ModelDiscovery
	if bound.Contract().Has(ModelDiscovery) {
		t.Fatal("binding exposed mutable capabilities")
	}
	plain := fixture{"plain", "/plain"}
	if Bind(plain, ctx) != plain || Bind(nil, ctx) != nil {
		t.Fatal("binding changed an existing codec")
	}
	var inbound Inbound = bound
	if _, err := inbound.Decode(ctx, http.Header{}, nil); err != nil {
		t.Fatal(err)
	}
}
