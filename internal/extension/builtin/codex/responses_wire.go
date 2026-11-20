package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/klauspost/compress/zstd"

	"github.com/yetone/magpie/internal/lazyre"
	sdk "github.com/yetone/magpie/pkg/extension"
)

// ResponsesWireReply retains the original stream unless the ChatGPT edge's
// reset calls for a replacement JSON error.
type ResponsesWireReply struct {
	Body      io.ReadCloser
	ResetJSON []byte
}

// ResponsesWireSend lends one synchronous send at the destination and with
// the context and account already selected by the host. A nil argument
// sends the existing signed body's bytes unchanged.
type ResponsesWireSend func(zstdBody []byte) (status int, body io.ReadCloser, err error)

type ResponsesWireMode uint8

const (
	ResponsesWireRoutedTurn ResponsesWireMode = iota
	ResponsesWireRelayedTurn
	ResponsesWireNativeCompact
)

func ExecuteResponsesWire(request sdk.ExecuteRequest, encoding string, mode ResponsesWireMode, send ResponsesWireSend) (ResponsesWireReply, error) {
	if request.Protocol != "responses" || request.Operation != "" {
		return ResponsesWireReply{}, errors.New("Codex wire execution requires a Responses request")
	}
	var zstdBody []byte
	switch mode {
	case ResponsesWireRoutedTurn, ResponsesWireRelayedTurn:
		zstdBody = EncodeResponsesZstd(request.Body, encoding)
	case ResponsesWireNativeCompact:
	default:
		return ResponsesWireReply{}, errors.New("unknown Codex Responses wire mode")
	}
	status, body, err := send(zstdBody)
	if err != nil {
		return ResponsesWireReply{}, err
	}
	if mode != ResponsesWireRoutedTurn {
		return ResponsesWireReply{Body: body}, nil
	}
	size := len(request.Body)
	if zstdBody != nil {
		size = len(zstdBody)
	}
	return ReadResponsesDrop(status, size, body), nil
}

// The encoder matches Codex CLI's level 3, checksum-free, 2 MiB window.
// With a coder per core, zstd histories held 74 MiB of a live gateway's
// heap on a 14-core Mac. Two low-memory coders retain about 9 MiB and keep
// up with ordinary turns without changing the bytes sent.
var responsesZstd = newResponsesZstd()

func newResponsesZstd() *zstd.Encoder {
	z, _ := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderCRC(false),
		zstd.WithWindowSize(2<<20),
		zstd.WithEncoderConcurrency(2),
		zstd.WithLowerEncoderMem(true),
	)
	return z
}

var responsesZstdOff atomic.Bool

func ResponsesZstdForTests(on bool) (restore func()) {
	was := responsesZstdOff.Swap(!on)
	return func() { responsesZstdOff.Store(was) }
}

func EncodeResponsesZstd(body []byte, encoding string) []byte {
	if responsesZstdOff.Load() || (encoding != "" && !strings.EqualFold(encoding, "identity")) {
		return nil
	}
	return responsesZstd.EncodeAll(body, make([]byte, 0, len(body)/8+64))
}

// The ChatGPT edge returns a 503 when the backend drops before headers and
// a 507 when the edge cannot buffer the request to retry that failure.
var edgeReset = lazyre.New(`(?i)upstream connect error or disconnect/reset before headers`)
var edgeUnbuffered = lazyre.New(`(?i)exceeded request buffer limit while retrying upstream`)

const resetSaid = "This usually passes, so magpie asked again without resting the account."

// The size is a clue for the 503, not a body cap. The 507 already says the
// edge's retry buffer was exceeded.
func EdgeMayDrop(status, size int) bool {
	return status == http.StatusInsufficientStorage || (status == http.StatusServiceUnavailable && size >= 1<<20)
}

func EdgeSaidDrop(status int, head []byte) bool {
	if status == http.StatusInsufficientStorage {
		return edgeUnbuffered.Match(head)
	}
	return edgeReset.Match(head)
}

func ReadResponsesDrop(status, size int, body io.ReadCloser) ResponsesWireReply {
	if body == nil || !EdgeMayDrop(status, size) {
		return ResponsesWireReply{Body: body}
	}
	head, _ := io.ReadAll(io.LimitReader(body, 4<<10))
	if !EdgeSaidDrop(status, head) {
		return ResponsesWireReply{Body: struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(head), body), body}}
	}
	body.Close()
	what := "ChatGPT's Codex backend closed the connection before answering this %.1f MiB request (503: %s). "
	if status == http.StatusInsufficientStorage {
		what = "ChatGPT's Codex backend failed this %.1f MiB request, and its front couldn't try again, as the request is more than it holds to retry (507: %s). "
	}
	msg := fmt.Sprintf(what+
		resetSaid+" "+
		"If it keeps happening in this conversation, its images may be why: run /compact, or remove or shrink the images (a JPEG in place of a large PNG screenshot).",
		float64(size)/(1<<20), bytes.TrimSpace(head))
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": "server_error"}})
	return ResponsesWireReply{ResetJSON: b}
}

func ResponseReset(status int, body []byte) bool {
	return status == http.StatusServiceUnavailable && bytes.Contains(body, []byte(resetSaid))
}
