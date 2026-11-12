//go:build !darwin

package update

import (
	"context"
	"errors"
)

func SigningIdentities(context.Context) ([]SigningIdentity, error) {
	return nil, errors.New("local update signing is available only on macOS")
}

func UseSigning(context.Context, string, string) error {
	return errors.New("local update signing is available only on macOS")
}

func signLocalApp(context.Context, *SigningConfig, string, string) (signingSnapshot, signingSnapshot, error) {
	return signingSnapshot{}, signingSnapshot{}, errors.New("local update signing is available only on macOS")
}

func verifyLocalPair(context.Context, *SigningConfig, string, string) (signingSnapshot, signingSnapshot, error) {
	return signingSnapshot{}, signingSnapshot{}, errors.New("local update signing is available only on macOS")
}
