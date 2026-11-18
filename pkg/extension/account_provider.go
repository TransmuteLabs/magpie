package extension

// AccountOnly reports a service account with no inference surface. Admission
// requires API 1.11 and account-auth; an inference sibling in the same module
// does not grant this provider access to model discovery or execution.
func (provider ProviderDescriptor) AccountOnly() bool {
	return len(provider.Protocols) == 0 && len(provider.Models) == 0 && len(provider.Operations) == 0
}

func accountServiceProvider(manifest Manifest, provider ProviderDescriptor) bool {
	return manifest.Contract.Has(AccountAuth) && (manifest.Contract.Has(OutboundExecutor) ||
		manifest.Contract.Version.Minor >= 11 && provider.AccountOnly())
}

// ValidateInferenceProvider guards the provider-local role before either model
// discovery or execution. Account identity and generation remain host-owned.
func ValidateInferenceProvider(manifest Manifest, scope Scope) error {
	for _, provider := range manifest.Providers {
		if provider.ID == scope.Provider && provider.AccountOnly() {
			return &Error{Code: "method_not_found", Message: "account-only provider has no inference methods", Outcome: "not-started"}
		}
	}
	return nil
}
