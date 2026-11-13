package extension

import (
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/yetone/magpie/internal/netproxy"
)

func releaseHTTPClient() *http.Client {
	transport := &http.Transport{
		Proxy:                 netproxy.Func,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	netproxy.TrustEnvRoots(transport)
	return &http.Client{
		Transport: netproxy.Dispatch(transport),
		Timeout:   10 * time.Minute,
		CheckRedirect: func(request *http.Request, previous []*http.Request) error {
			if len(previous) >= 10 {
				return errors.New("extension release exceeded its redirect limit")
			}
			if request.URL.Scheme != "https" || request.URL.User != nil || request.URL.Fragment != "" {
				return errors.New("extension release redirect must use HTTPS without credentials or a fragment")
			}
			return nil
		},
	}
}
