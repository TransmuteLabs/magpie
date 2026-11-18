package extension

import "fmt"

const ActivationGateway = "gateway"

func (manifest Manifest) ActivatesGateway() bool {
	return len(manifest.Activation) == 1 && manifest.Activation[0] == ActivationGateway &&
		manifest.Contract.Version.Minor >= 8 && manifest.Component == nil
}

func (manifest Manifest) validateActivation() error {
	if len(manifest.Activation) == 0 {
		return nil
	}
	if !manifest.ActivatesGateway() {
		return fmt.Errorf("activation requires SDK 1.8 and the finite root gateway declaration")
	}
	return nil
}
