package protocolext

import (
	"encoding/json"
	"fmt"
)

type ExecutorPolicy string

const SameModuleExecutor ExecutorPolicy = "same-module"

func (policy *ExecutorPolicy) UnmarshalJSON(raw []byte) error {
	var value *string
	if err := json.Unmarshal(raw, &value); err != nil || value == nil ||
		*value != "" && ExecutorPolicy(*value) != SameModuleExecutor {
		return fmt.Errorf("invalid executor policy")
	}
	*policy = ExecutorPolicy(*value)
	return nil
}

func ExecutorContract() Contract {
	return Contract{
		Version:      APIVersion{Major: Version, Minor: 4},
		Capabilities: []Capability{InboundCodec, ExecutorBinding},
	}
}

func ValidateExecutor(policy ExecutorPolicy, protocol Protocol) error {
	if policy == "" {
		return nil
	}
	if policy == SameModuleExecutor {
		switch protocol {
		case Chat, Responses, Messages, Anthropic, Gemini:
			return nil
		}
	}
	return fmt.Errorf("invalid executor policy for the canonical operation")
}
