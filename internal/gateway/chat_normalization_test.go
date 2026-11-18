package gateway

import (
	"bytes"
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/pkg/protocolext"
)

func TestBuiltinChatNormalizationDTO(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	headers := http.Header{"X-Tenant": {"original"}}
	originalHeaders := headers.Clone()
	for _, body := range []string{
		`{"model":"mock-model","messages":[{"role":"user","content":"你好"}],"unknown":123.450}`,
		`{"reasoning_effort":"low","prompt_cache_key":"own","tools":[{"type":"function","function":{"name":"keep","parameters":{"type":"object"}}}]}`,
		`not JSON`,
	} {
		decoded, err := (chatNormalizer{}).Decode(ctx, headers, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(decoded.Body, []byte(body)) {
			t.Errorf("canonical or opaque body changed: %s", decoded.Body)
		}
		decoded.Body = nil
		if !reflect.DeepEqual(decoded, protocolext.Request{Protocol: protocolext.Chat}) {
			t.Errorf("decoder acquired request authority: %+v", decoded)
		}
	}
	if !reflect.DeepEqual(headers, originalHeaders) {
		t.Fatalf("decoder mutated caller headers: %v", headers)
	}
	body := []byte(`{"thinking":{"effort":"high"},"promptCacheKey":"cached","messages":[{"role":"system","tools":[{"name":"lookup","inputSchema":{"type":"object"}}]},{"role":"user","content":"hello"}]}`)
	decoded, err := (chatNormalizer{}).Decode(ctx, headers, body)
	if err != nil {
		t.Fatal(err)
	}
	got := gjson.ParseBytes(decoded.Body)
	if got.Get("reasoning_effort").String() != "high" || got.Get("thinking.effort").Exists() {
		t.Errorf("effort normalization: %s", decoded.Body)
	}
	if got.Get("prompt_cache_key").String() != "cached" || got.Get("promptCacheKey").Exists() {
		t.Errorf("cache normalization: %s", decoded.Body)
	}
	if got.Get("tools.0.type").String() != "function" || got.Get("tools.0.function.name").String() != "lookup" || got.Get("tools.0.function.parameters.type").String() != "object" {
		t.Errorf("lift-before-shape normalization: %s", decoded.Body)
	}
	if got.Get("messages.#").Int() != 1 || got.Get("messages.0.role").String() != "user" {
		t.Errorf("tool-only message retained: %s", decoded.Body)
	}
}
