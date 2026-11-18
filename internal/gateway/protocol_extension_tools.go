package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	sdk "github.com/yetone/magpie/pkg/extension"
	codec "github.com/yetone/magpie/pkg/protocolext"
)

// Native control RPCs use the existing MCP tool policy and budget path. A
// codec can select only a tool its own installed module has declared.
func (server *Server) protocolToolBody(request *http.Request, inbound codec.Inbound, body []byte) ([]byte, error) {
	request = request.WithContext(extensionToolHTTPContext(request))
	owner, _, named := strings.Cut(inbound.ID(), ":")
	if !named || server.extensions == nil || !inbound.Contract().Has(codec.ToolProvider) {
		return nil, errors.New("native codec did not negotiate tool dispatch")
	}
	manifest, err := server.extensions.Manifest(owner)
	if err != nil || !manifest.Contract.Has(sdk.ToolProvider) {
		return nil, errors.New("native module tools are unavailable")
	}
	var call sdk.ToolRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&call) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("native codec produced an invalid tool request")
	}
	if len(call.Arguments) == 0 {
		call.Arguments = json.RawMessage(`{}`)
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(call.Arguments, &args) != nil || args == nil {
		return nil, errors.New("native tool arguments must be a JSON object")
	}
	declared := false
	for _, tool := range manifest.Tools {
		if tool.Name == call.ID {
			declared = true
			break
		}
	}
	if !declared {
		return nil, errors.New("native codec selected an undeclared tool")
	}
	tools, err := servedTools(request.Context())
	if err != nil {
		return nil, err
	}
	for _, tool := range tools {
		if tool.spec == "extension:"+owner && tool.Tool.Name == call.ID {
			return json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": 1, "method": "tools/call",
				"params": map[string]any{"name": tool.name, "arguments": call.Arguments},
			})
		}
	}
	return nil, errors.New("native module tools are not enabled")
}
