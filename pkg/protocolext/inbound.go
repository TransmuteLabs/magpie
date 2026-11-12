package protocolext

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"reflect"
	"slices"
	"strings"
	"time"
)

const Version = 0

type Protocol string

const (
	Chat   Protocol = "chat"
	Models Protocol = "models"
)

type Request struct {
	Protocol Protocol
	Body     []byte
	Session  string
	Timeout  time.Duration
}

type Inbound interface {
	Contract() Contract
	ID() string
	Path() string
	Decode(context.Context, http.Header, []byte) (Request, error)
	Reply(http.ResponseWriter) Response
}

type Response interface {
	http.ResponseWriter
	Finish(cause error) error
}

type Error struct {
	Status  int
	Message string
}

func (err *Error) Error() string { return err.Message }

type Registry struct {
	paths map[string]Inbound
}

type registeredInbound struct {
	Inbound
	contract Contract
	id       string
	path     string
}

func (entry registeredInbound) Contract() Contract {
	snapshot := entry.contract
	snapshot.Capabilities = slices.Clone(snapshot.Capabilities)
	return snapshot
}

func (entry registeredInbound) ID() string   { return entry.id }
func (entry registeredInbound) Path() string { return entry.path }

func New(inbounds ...Inbound) (*Registry, error) {
	registry := &Registry{paths: make(map[string]Inbound, len(inbounds))}
	ids := make(map[string]bool, len(inbounds))
	for _, inbound := range inbounds {
		if inbound == nil || reflect.ValueOf(inbound).Kind() == reflect.Pointer && reflect.ValueOf(inbound).IsNil() {
			return nil, fmt.Errorf("nil inbound adapter")
		}
		contract := inbound.Contract()
		if err := HostContract().Check(contract); err != nil {
			return nil, err
		}
		if !contract.Has(InboundCodec) {
			return nil, fmt.Errorf("inbound adapter does not declare its codec contract")
		}
		id, endpoint := inbound.ID(), inbound.Path()
		if id == "" || strings.ContainsAny(id, " /\\\t\r\n") {
			return nil, fmt.Errorf("invalid adapter id %q", id)
		}
		if endpoint == "/" || !strings.HasPrefix(endpoint, "/") || path.Clean(endpoint) != endpoint || strings.ContainsAny(endpoint, "{}?%#\\\t\r\n ") {
			return nil, fmt.Errorf("invalid adapter path %q", endpoint)
		}
		if ids[id] || registry.paths[endpoint] != nil {
			return nil, fmt.Errorf("duplicate adapter id or path: %q", id)
		}
		contract.Capabilities = slices.Clone(contract.Capabilities)
		ids[id] = true
		registry.paths[endpoint] = registeredInbound{Inbound: inbound, contract: contract, id: id, path: endpoint}
	}
	return registry, nil
}

func (registry *Registry) Lookup(endpoint string) (Inbound, bool) {
	if registry == nil {
		return nil, false
	}
	inbound, found := registry.paths[endpoint]
	return inbound, found
}

func (registry *Registry) Paths() []string {
	if registry == nil {
		return nil
	}
	endpoints := make([]string, 0, len(registry.paths))
	for endpoint := range registry.paths {
		endpoints = append(endpoints, endpoint)
	}
	slices.Sort(endpoints)
	return endpoints
}
