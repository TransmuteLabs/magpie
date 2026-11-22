package gateway

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

func TestGatewayHookReplyP19UnaryAllocations(t *testing.T) {
	body := []byte(p19HookReplyBody(4 << 10))
	var reply json.RawMessage
	var err error
	allocations := testing.AllocsPerRun(20, func() {
		reply, err = gatewayReply(provider.Responses, body)
	})
	if err != nil || !bytes.Equal(reply, body) {
		t.Fatalf("gatewayReply did not preserve the unary reply: length %d, error %v", len(reply), err)
	}
	if allocations > 4 {
		t.Errorf("gatewayReply allocated %.0f times per unary reply, want at most 4", allocations)
	}
}
