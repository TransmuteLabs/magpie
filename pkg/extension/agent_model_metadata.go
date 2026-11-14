package extension

import (
	"errors"
	"slices"
)

type AgentModelMetadata struct {
	MaxOutput     int64    `json:"maxOutput,omitempty"`
	Efforts       []string `json:"efforts"`
	DefaultEffort string   `json:"defaultEffort,omitempty"`
}

func (value AgentModelMetadata) Clone() AgentModelMetadata {
	value.Efforts = slices.Clone(value.Efforts)
	return value
}

func ValidateAgentModelMetadata(manifest Manifest, value AgentModelMetadata) error {
	if manifest.Contract.Version.Major != 1 || manifest.Contract.Version.Minor < 9 || !manifest.Contract.Has(AgentWiring) {
		return errors.New("effective agent model metadata requires SDK API 1.9 agent-wiring")
	}
	if value.MaxOutput < 0 || value.MaxOutput > 9007199254740991 || len(value.Efforts) > 8 {
		return errors.New("invalid agent model limits or effort levels")
	}
	seen := map[string]bool{}
	for _, effort := range value.Efforts {
		switch effort {
		case "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
		default:
			return errors.New("unsupported agent model effort level")
		}
		if seen[effort] {
			return errors.New("duplicate agent model effort level")
		}
		seen[effort] = true
	}
	if value.DefaultEffort != "" && !seen[value.DefaultEffort] {
		return errors.New("agent model default must belong to its effective effort levels")
	}
	return nil
}
