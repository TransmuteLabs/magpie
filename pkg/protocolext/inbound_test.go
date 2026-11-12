package protocolext

import (
	"context"
	"net/http"
	"slices"
	"testing"
)

type fixture struct{ id, path string }

func (fixture) Contract() Contract   { return InboundContract() }
func (adapter fixture) ID() string   { return adapter.id }
func (adapter fixture) Path() string { return adapter.path }
func (fixture) Decode(context.Context, http.Header, []byte) (Request, error) {
	return Request{}, nil
}
func (fixture) Reply(http.ResponseWriter) Response { return nil }

func TestRegistryRejectsAmbiguousRoutes(t *testing.T) {
	for _, endpoint := range []string{"", "/", "rpc", "/rpc/", "/rpc//method", "/rpc/../api", "/{path...}", "/rpc?x=y", "/rpc%2fmethod", "/rpc\\method", "/rpc\nmethod"} {
		if _, err := New(fixture{"codec", endpoint}); err == nil {
			t.Errorf("accepted ambiguous route %q", endpoint)
		}
	}
	var absent *mutableContract
	for _, entries := range [][]Inbound{
		{nil},
		{absent},
		{fixture{"", "/rpc"}},
		{fixture{"one", "/rpc"}, fixture{"two", "/rpc"}},
		{fixture{"one", "/rpc"}, fixture{"one", "/other"}},
	} {
		if _, err := New(entries...); err == nil {
			t.Errorf("accepted invalid adapter set: %v", entries)
		}
	}
}

func TestRegistryOnlyMatchesExactEndpoint(t *testing.T) {
	registry, err := New(fixture{"codec", "/rpc/method"})
	if err != nil {
		t.Fatal(err)
	}
	if inbound, found := registry.Lookup("/rpc/method"); !found || inbound.ID() != "codec" {
		t.Fatal("missing registered adapter")
	}
	for _, endpoint := range []string{"/rpc", "/rpc/method/", "/rpc/method/extra", "/rpc//method", "/rpc/%6dethod", "/api/state"} {
		if _, found := registry.Lookup(endpoint); found {
			t.Errorf("matched %q", endpoint)
		}
	}
	var absent *Registry
	if _, found := absent.Lookup("/rpc/method"); found || len(absent.Paths()) != 0 {
		t.Fatal("nil registry matched")
	}
}

func TestRegistryPathsAreIndependentSnapshots(t *testing.T) {
	registry, err := New(fixture{"last", "/z/rpc"}, fixture{"first", "/a/rpc"})
	if err != nil {
		t.Fatal(err)
	}
	paths := registry.Paths()
	if !slices.Equal(paths, []string{"/a/rpc", "/z/rpc"}) {
		t.Fatal(paths)
	}
	paths[0] = "/api/state"
	if !slices.Equal(registry.Paths(), []string{"/a/rpc", "/z/rpc"}) {
		t.Fatal("caller changed registry paths")
	}
}
