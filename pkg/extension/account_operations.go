package extension

import (
	"fmt"
	"net/http"
	"slices"
)

type AccountOperationDescriptor struct {
	ID       string `json:"id"`
	Agent    string `json:"agent"`
	Provider string `json:"provider"`
	Action   string `json:"action"`
}

type AccountOperationRequest struct {
	Model    string `json:"model"`
	Body     []byte `json:"body,omitempty"`
	CallerID string `json:"callerId,omitempty"`
}

type AccountOperationResult struct {
	Status  int         `json:"status"`
	Headers http.Header `json:"headers,omitempty"`
	Body    []byte      `json:"body,omitempty"`
}

func (manifest Manifest) AccountOperation(id string) (AccountOperationDescriptor, bool) {
	for _, operation := range manifest.AccountOperations {
		if operation.ID == id {
			return operation, true
		}
	}
	return AccountOperationDescriptor{}, false
}

func (manifest Manifest) validateAccountOperations() error {
	enabled := manifest.Contract.Has(NativeAccountOperations)
	if len(manifest.AccountOperations) == 0 {
		if enabled {
			return fmt.Errorf("native account operations need finite declarations")
		}
		return nil
	}
	if !enabled || manifest.Contract.Version.Minor < 6 || len(manifest.AccountOperations) > 32 ||
		!manifest.Contract.Has(InboundCodec) || !manifest.Contract.Has(OutboundExecutor) ||
		!manifest.Contract.Has(AccountAuth) || !manifest.Contract.Has(AgentWiring) || !manifest.Contract.Has(ModuleRPC) {
		return fmt.Errorf("native account operations require their SDK 1.6 capabilities")
	}
	seen := map[string]bool{}
	for _, operation := range manifest.AccountOperations {
		action, found := manifest.ResolveAction(operation.Action)
		agent := slices.IndexFunc(manifest.Agents, func(value AgentDescriptor) bool { return value.ID == operation.Agent })
		provider := slices.IndexFunc(manifest.Providers, func(value ProviderDescriptor) bool { return value.ID == operation.Provider })
		if !validID(operation.ID) || seen[operation.ID] ||
			agent < 0 || provider < 0 || !found || actionOwner(action) != PrimaryComponent ||
			action.Effect != "read" && action.Effect != "write" {
			return fmt.Errorf("invalid native account operation declaration")
		}
		descriptor := manifest.Agents[agent]
		if descriptor.Launch == nil || descriptor.Launch.Model == "" {
			return fmt.Errorf("native account operation needs a model-bound agent launch")
		}
		seen[operation.ID] = true
	}
	return nil
}
