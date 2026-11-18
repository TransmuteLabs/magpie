package middleware

import (
	"errors"
	"net/http"

	"github.com/yetone/magpie/internal/gwmw"
)

type ResponseHooks struct {
	Event    func(string, []byte) ([]byte, bool)
	Response func([]byte, int) []byte
	MaxBytes int
}

func WrapResponse(w http.ResponseWriter, hooks ResponseHooks) (http.ResponseWriter, func()) {
	if hooks.Event == nil && hooks.Response == nil {
		return w, func() {}
	}
	writer := &writer{w: w, hooks: hooks, ev: hooks.Event != nil, resp: hooks.Response != nil, status: http.StatusOK}
	return writer, writer.finish
}

func AccountReference(module string, account gwmw.Account) string {
	return accountRef("sdk:"+module, account)
}

func (run *Run) responseBody(body []byte, status int) []byte {
	for index, module := range run.set.list {
		if !module.hasResp {
			continue
		}
		out, err := run.apply(index, module.onResp, requestLimit, body, &status)
		switch {
		case errors.Is(err, errDropped):
			module.fail("onResponse", errors.New("returned null; a whole reply can't be dropped"))
		case err != nil:
			module.fail("onResponse", err)
		case out != nil:
			body = out
		}
	}
	return body
}

func (run *Run) eventData(name string, data []byte) ([]byte, bool) {
	changed := false
	for index, module := range run.set.list {
		if !module.hasEv || module.events != nil && !module.events[name] {
			continue
		}
		out, err := run.apply(index, module.onEv, eventLimit, data, nil)
		switch {
		case errors.Is(err, errDropped):
			return nil, true
		case err != nil:
			module.fail("onEvent", err)
		case out != nil:
			data, changed = out, true
		}
	}
	if !changed {
		return nil, false
	}
	return data, false
}
