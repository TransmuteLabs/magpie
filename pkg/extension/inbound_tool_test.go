package extension

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yetone/magpie/pkg/protocolext"
)

func TestInboundToolRequiresBothModuleAndFrozenCodecCapability(t *testing.T) {
	for _, test := range []struct {
		name       string
		moduleTool bool
		codecTool  bool
	}{
		{name: "both", moduleTool: true, codecTool: true},
		{name: "module only", moduleTool: true},
		{name: "codec only", codecTool: true},
		{name: "neither"},
	} {
		t.Run(test.name, func(t *testing.T) {
			contract := protocolext.CatalogContract()
			if test.codecTool {
				contract = protocolext.ToolContract()
			}
			body, err := json.Marshal(ToolRequest{ID: "job", Arguments: json.RawMessage(`{"action":"poll","job":"native"}`)})
			if err != nil {
				t.Fatal(err)
			}
			codec := &fixtureCodec{contract: contract, protocol: protocolext.Tool, decodedBody: body}
			module := inboundFixtureModule(codec)
			module.Manifest.Contract.Capabilities = []Capability{InboundCodec}
			if test.moduleTool {
				module.Manifest.Contract.Capabilities = append(module.Manifest.Contract.Capabilities, ToolProvider)
				module.Manifest.Tools = []ToolDescriptor{{Name: "job", InputSchema: json.RawMessage(`{"type":"object"}`)}}
				module.Handlers = map[string]Handler{MethodToolCall: testNoop}
			}
			peer := startPeer(t, module)
			result, _ := completeRPC(t, peer, callFrame("decode", MethodInboundDecode, NativeRequest{ID: "codec", ResponseID: "native", Body: []byte("control")}))
			if test.moduleTool && test.codecTool {
				var decoded DecodedRequest
				if result.Error != nil || json.Unmarshal(result.Result, &decoded) != nil || decoded.Protocol != "tool" || string(decoded.Body) != string(body) ||
					decoded.RequestID != "native-operation" || string(decoded.Metadata) != `{"option":"native"}` {
					t.Fatalf("native tool operation was not retained: %+v decoded=%+v", result, decoded)
				}
			} else if result.Error == nil || result.Error.Code != "permission_denied" {
				t.Fatalf("tool invocation gained unnegotiated authority: %+v", result)
			}
		})
	}
}

func TestInboundToolRefusesInvalidForeignAndOversizedBodies(t *testing.T) {
	for _, test := range []struct {
		name, body string
	}{
		{name: "malformed", body: `{`},
		{name: "foreign tool", body: `{"id":"another-plugin/job","arguments":{}}`},
		{name: "undeclared tool", body: `{"id":"unknown","arguments":{}}`},
		{name: "missing arguments", body: `{"id":"job"}`},
		{name: "extra target", body: `{"id":"job","arguments":{},"target":"/api/admin"}`},
		{name: "oversized", body: `{"id":"job","arguments":{"data":"` + strings.Repeat("x", MaxNativeBodyBytes) + `"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			codec := &fixtureCodec{contract: protocolext.ToolContract(), protocol: protocolext.Tool, decodedBody: []byte(test.body)}
			module := inboundFixtureModule(codec)
			module.Manifest.Contract.Capabilities = []Capability{InboundCodec, ToolProvider}
			module.Manifest.Tools = []ToolDescriptor{{Name: "job", InputSchema: json.RawMessage(`{"type":"object"}`)}}
			module.Handlers = map[string]Handler{MethodToolCall: testNoop}
			peer := startPeer(t, module)
			result, _ := completeRPC(t, peer, callFrame("decode", MethodInboundDecode, NativeRequest{ID: "codec", ResponseID: "native", Body: []byte("control")}))
			if result.Error == nil {
				t.Fatal("invalid tool body was admitted", test.name)
			}
		})
	}
	manifest := testManifest(OutboundExecutor)
	manifest.Providers = []ProviderDescriptor{providerDescriptor()}
	manifest.Providers[0].Protocols = []protocolext.Protocol{protocolext.Tool}
	if manifest.Validate() == nil {
		t.Fatal("tool invocation became a provider LLM wire protocol")
	}
}
