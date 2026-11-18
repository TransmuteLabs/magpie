package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
	codec "github.com/yetone/magpie/pkg/protocolext"
)

type ProtocolAgentBinding struct {
	Model    string
	Revision string
	Check    func(context.Context) error
}

type ProtocolAgentBindingResolver func(context.Context, *extension.Lease, string) (ProtocolAgentBinding, error)

var protocolAgentResolver struct {
	sync.RWMutex
	resolve ProtocolAgentBindingResolver
}

func RegisterProtocolAgentBindingResolver(resolve ProtocolAgentBindingResolver) {
	protocolAgentResolver.Lock()
	defer protocolAgentResolver.Unlock()
	if resolve == nil || protocolAgentResolver.resolve != nil {
		panic("native agent binding resolver must have one host owner")
	}
	protocolAgentResolver.resolve = resolve
}

func nativeAccountCaller(request *http.Request) error {
	if !extension.LocalOwner(extensionToolHTTPContext(request)) {
		return errors.New("native account operations require the local owner")
	}
	who := access.Caller(request.Context())
	for held := &who; held != nil; held = held.Issuer {
		if held.ReadOnly || held.Browser || held.Restricted() || len(held.Limits()) != 0 || held.MaxConcurrency > 0 {
			return errors.New("this caller cannot delegate native account credentials")
		}
	}
	return nil
}

func (server *Server) protocolAccountOperation(writer http.ResponseWriter, request *http.Request, inbound codec.Inbound, decoded codec.Request) error {
	if server.extensions == nil || !inbound.Contract().Has(codec.NativeAccountOperations) ||
		decoded.Executor != codec.SameModuleExecutor || len(decoded.Body) > sdk.MaxHostBytes {
		return errors.New("native account operation was not negotiated")
	}
	if err := nativeAccountCaller(request); err != nil {
		return err
	}
	ctx, err := extension.WithExecutorPolicy(request.Context(), decoded.Executor)
	if err != nil {
		return err
	}
	lease, err := extension.AccountOperationLease(ctx)
	if err != nil {
		return err
	}
	manifest := lease.Manifest()
	operation, declared := manifest.AccountOperation(decoded.AccountOperation)
	owner, _, named := strings.Cut(inbound.ID(), ":")
	if !declared || !named || owner != manifest.ID {
		return errors.New("native account operation has no installed owner")
	}
	protocolAgentResolver.RLock()
	resolve := protocolAgentResolver.resolve
	protocolAgentResolver.RUnlock()
	if resolve == nil {
		return errors.New("native agent binding resolution is unavailable")
	}
	binding, err := resolve(ctx, lease, operation.Agent)
	if err != nil || binding.Check == nil || binding.Model == "" || binding.Revision == "" {
		return errors.New("native account operation needs its current owned agent binding")
	}
	model := strings.TrimPrefix(binding.Model, "magpie/")
	if !validProtocolModel(model) || provider.IsGroupRef(model) {
		return errors.New("native account operation requires an explicit provider model")
	}
	shown, _ := provider.CatalogFor(operation.Agent)
	if !slices.ContainsFunc(shown, func(entry provider.Entry) bool { return entry.ID == model }) {
		return errors.New("native account operation model is not visible to its agent")
	}
	selected, nativeModel, found := provider.ResolveRoute(model)
	if !found || !selected.IsExtension() || !selected.On() || selected.OwnPaused() ||
		!selected.AccountServes(selected.Account.User, nativeModel) {
		return errors.New("native account operation account is unavailable")
	}
	ctx, scope, err := selected.NativeAccountScope(ctx, lease, operation.Provider)
	if err != nil {
		return err
	}
	if len(decoded.Session) > 256 || strings.ContainsAny(decoded.Session, "\r\n\x00") {
		return errors.New("native account operation session identity is invalid")
	}
	scope.Session = decoded.Session
	check := func() error {
		if err := lease.CheckCurrent(ctx); err != nil {
			return err
		}
		if err := binding.Check(ctx); err != nil {
			return err
		}
		_, current, err := selected.NativeAccountScope(ctx, lease, operation.Provider)
		if err != nil || current.Generation != scope.Generation || current.Account != scope.Account {
			return errors.New("native account operation selection changed")
		}
		return nil
	}
	if err := check(); err != nil {
		return err
	}
	action, exists := manifest.ResolveAction(operation.Action)
	if !exists {
		return errors.New("native account operation action is unavailable")
	}
	if action.Effect == "write" {
		if decoded.RequestID == "" || len(decoded.RequestID) > 256 || strings.ContainsAny(decoded.RequestID, "\r\n\x00") {
			return errors.New("native account write requires its stable request identity")
		}
		identity, err := json.Marshal([]string{manifest.ID, operation.ID, binding.Revision, scope.Provider,
			scope.Account, extension.CallerIDFrom(ctx), decoded.RequestID})
		if err != nil {
			return err
		}
		digest := sha256.Sum256(identity)
		scope.Operation, scope.Attempt = "native-account-"+hex.EncodeToString(digest[:]), 1
	}
	input, err := json.Marshal(sdk.AccountOperationRequest{Model: nativeModel, Body: bytes.Clone(decoded.Body),
		CallerID: extension.CallerIDFrom(ctx)})
	if err != nil {
		return err
	}
	result, err := lease.Call(ctx, sdk.MethodModuleCall, scope,
		sdk.ModuleCallRequest{Action: operation.Action, Input: input}, nil)
	if err != nil {
		return err
	}
	if err := check(); err != nil {
		return err
	}
	var response sdk.AccountOperationResult
	if json.Unmarshal(result, &response) != nil || response.Status < 200 || response.Status > 599 ||
		len(response.Body) > sdk.MaxHostBytes {
		return errors.New("native account operation returned an invalid response")
	}
	for name, values := range response.Headers {
		switch http.CanonicalHeaderKey(name) {
		case "Content-Type", "Retry-After":
			for _, value := range values {
				if len(value) > 1024 || strings.ContainsAny(value, "\r\n\x00") {
					return fmt.Errorf("native account operation returned invalid headers")
				}
			}
			writer.Header()[http.CanonicalHeaderKey(name)] = slices.Clone(values)
		}
	}
	writer.WriteHeader(response.Status)
	_, err = writer.Write(response.Body)
	return err
}
