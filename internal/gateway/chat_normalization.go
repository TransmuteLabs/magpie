package gateway

import (
	"context"
	"net/http"

	"github.com/yetone/magpie/pkg/protocolext"
)

type chatNormalizer struct{}

var _ func(context.Context, http.Header, []byte) (protocolext.Request, error) = chatNormalizer{}.Decode

func (chatNormalizer) Decode(_ context.Context, _ http.Header, body []byte) (protocolext.Request, error) {
	body = thinkingEffort(body)
	body = cacheKeyCamel(body)
	body = liftMessageTools(body)
	body = shapeChatTools(body)
	return protocolext.Request{Protocol: protocolext.Chat, Body: body}, nil
}
