package protocolext

import (
	"fmt"

	sdk "github.com/yetone/magpie/pkg/protocolext"
)

type Host struct {
	registry *sdk.Registry
}

func New(registry *sdk.Registry, reserved func(string) bool) (*Host, error) {
	if reserved == nil {
		return nil, fmt.Errorf("protocol host requires a route ownership check")
	}
	for _, endpoint := range registry.Paths() {
		if reserved(endpoint) {
			return nil, fmt.Errorf("protocol endpoint %q belongs to the host", endpoint)
		}
	}
	return &Host{registry: registry}, nil
}

func (host *Host) Lookup(endpoint string) (sdk.Inbound, bool) {
	if host == nil {
		return nil, false
	}
	return host.registry.Lookup(endpoint)
}

func (host *Host) ClientPath(endpoint string) bool {
	_, found := host.Lookup(endpoint)
	return found
}
