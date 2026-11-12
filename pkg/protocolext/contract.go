package protocolext

import "fmt"

type APIVersion struct {
	Major int `json:"major"`
	Minor int `json:"minor"`
}

type Capability string

const (
	InboundCodec     Capability = "inbound-codec"
	OutboundExecutor Capability = "outbound-executor"
	AgentWiring      Capability = "agent-wiring"
	AccountAuth      Capability = "account-auth"
	ModelDiscovery   Capability = "model-discovery"
	UsageReporter    Capability = "usage-reporter"
	OperationControl Capability = "operation-control"
	ToolProvider     Capability = "tool-provider"
)

type Contract struct {
	Version      APIVersion   `json:"version"`
	Capabilities []Capability `json:"capabilities"`
}

func (host Contract) Check(module Contract) error {
	if module.Version.Major != host.Version.Major || module.Version.Minor < 0 || module.Version.Minor > host.Version.Minor {
		return fmt.Errorf("unsupported protocol extension contract version %d.%d", module.Version.Major, module.Version.Minor)
	}
	if len(module.Capabilities) == 0 {
		return fmt.Errorf("protocol extension declares no capabilities")
	}
	available := make(map[Capability]bool, len(host.Capabilities))
	for _, capability := range host.Capabilities {
		available[capability] = true
	}
	seen := make(map[Capability]bool, len(module.Capabilities))
	for _, capability := range module.Capabilities {
		if !available[capability] || seen[capability] {
			return fmt.Errorf("unsupported or duplicate protocol extension capability %q", capability)
		}
		seen[capability] = true
	}
	return nil
}

func InboundContract() Contract {
	return Contract{Version: APIVersion{Major: Version, Minor: 1}, Capabilities: []Capability{InboundCodec}}
}

func CatalogContract() Contract {
	return Contract{Version: APIVersion{Major: Version, Minor: 2}, Capabilities: []Capability{InboundCodec, ModelDiscovery}}
}

func ToolContract() Contract {
	return Contract{Version: APIVersion{Major: Version, Minor: 3}, Capabilities: []Capability{InboundCodec, ToolProvider}}
}

func HostContract() Contract {
	return Contract{Version: APIVersion{Major: Version, Minor: 3}, Capabilities: []Capability{InboundCodec, ModelDiscovery, ToolProvider}}
}

func (contract Contract) Has(capability Capability) bool {
	for _, declared := range contract.Capabilities {
		if declared == capability {
			return true
		}
	}
	return false
}
