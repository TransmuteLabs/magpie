// Package hybrid is shared by this example's native controller and Go service.
// It imports only public Magpie SDK packages and the standard library.
package hybrid

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"os"
	"runtime"

	sdk "github.com/yetone/magpie/pkg/extension"
)

// Version is set for both components by build.py's -ldflags argument.
var Version = "1.0.0"

//go:embed magpie-plugin.json
var declaration []byte

func Manifest() sdk.Manifest {
	var manifest sdk.Manifest
	if err := json.Unmarshal(declaration, &manifest); err != nil {
		panic(err)
	}
	manifest.Version = Version
	switch runtime.GOOS {
	case "darwin":
		manifest.Entry = "lib/primary.dylib"
	case "windows":
		manifest.Entry = "lib/primary.dll"
		manifest.Services[0].Entry = "bin/backend.exe"
	}
	if err := sdk.CheckCompatibility(manifest); err != nil {
		panic(err)
	}
	return manifest
}

type Config struct {
	Value     string `json:"value"`
	FailStart bool   `json:"failStart,omitempty"`
}

type Input struct {
	Text string `json:"text"`
	Wait bool   `json:"wait,omitempty"`
}

// Identity exposes a digest of fixture credentials, never the credentials.
type Identity struct {
	PID             int    `json:"pid"`
	Version         string `json:"version"`
	Component       string `json:"component"`
	Instance        string `json:"instance"`
	ConfigVersion   string `json:"configVersion"`
	Value           string `json:"value"`
	Text            string `json:"text"`
	Account         string `json:"account,omitempty"`
	Generation      uint64 `json:"generation,omitempty"`
	AuthFingerprint string `json:"authFingerprint,omitempty"`
	Endpoint        string `json:"endpoint,omitempty"`
	Outcome         string `json:"outcome,omitempty"`
}

type ForwardResult struct {
	Primary Identity `json:"primary"`
	Backend Identity `json:"backend"`
}

func Describe(ctx context.Context, call sdk.Call, text string) (Identity, error) {
	module, found := sdk.ModuleContextFrom(ctx)
	if !found {
		return Identity{}, &sdk.Error{Code: "not_ready", Message: "Hybrid component was not configured."}
	}
	var config Config
	if err := json.Unmarshal(module.Config.Values, &config); err != nil {
		return Identity{}, err
	}
	result := Identity{
		PID: os.Getpid(), Version: Version, Component: module.Component, Instance: module.Instance,
		ConfigVersion: module.Config.Version, Value: config.Value, Text: text,
		Account: call.Scope.Account, Generation: call.Scope.Generation,
	}
	if len(call.Auth) > 0 {
		hash := sha256.Sum256(call.Auth)
		result.AuthFingerprint = hex.EncodeToString(hash[:])
	}
	return result, nil
}

func Read(ctx context.Context, call sdk.Call, _ *sdk.Stream) (any, error) {
	var input Input
	if err := json.Unmarshal(call.Params, &input); err != nil {
		return nil, err
	}
	return Describe(ctx, call, input.Text)
}

func Accept(ctx context.Context, call sdk.Call, stream *sdk.Stream) error {
	return stream.Emit(ctx, "accepted", sdk.AcceptedEvent{OperationID: call.Scope.Operation})
}

func ToolContent(value any) (sdk.ToolResult, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return sdk.ToolResult{}, err
	}
	content, err := json.Marshal([]map[string]string{{"type": "text", "text": string(body)}})
	return sdk.ToolResult{Content: content}, err
}
