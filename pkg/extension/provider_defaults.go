package extension

import "fmt"

func ValidateProviderDefaults(manifest Manifest, provider ProviderDescriptor) error {
	for _, limit := range []*int{provider.MaxConcurrency, provider.MaxAccounts} {
		if limit == nil {
			continue
		}
		if manifest.Contract.Version.Minor < 9 {
			return fmt.Errorf("provider defaults require SDK API 1.9")
		}
		if *limit < 0 || *limit > 1000 {
			return fmt.Errorf("provider defaults must be between 0 and 1000")
		}
	}
	return nil
}
