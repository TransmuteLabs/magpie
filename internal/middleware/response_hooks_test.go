package middleware

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSDKResponseHooksSplitAndBounds(test *testing.T) {
	test.Run("stream", func(test *testing.T) {
		response := httptest.NewRecorder()
		calls := 0
		writer, finish := WrapResponse(response, ResponseHooks{MaxBytes: 1024,
			Event: func(name string, body []byte) ([]byte, bool) {
				calls++
				if name == "drop" {
					return nil, true
				}
				var value map[string]any
				if json.Unmarshal(body, &value) != nil {
					return nil, false
				}
				value["changed"] = true
				out, _ := json.Marshal(value)
				return out, false
			}})
		writer.Header().Set("Content-Type", "text/event-stream")
		input := ": ping\n\nevent: delta\r\ndata: {\"text\":\"hello\"}\r\n\r\nevent: drop\ndata: {\"drop\":true}\n\ndata: [DONE]\n\n"
		for _, value := range []byte(input) {
			if _, err := writer.Write([]byte{value}); err != nil {
				test.Fatal(err)
			}
		}
		finish()
		finish()
		output := response.Body.String()
		if calls != 2 || !strings.Contains(output, `"changed":true`) || !strings.Contains(output, ": ping\n\n") ||
			!strings.Contains(output, "[DONE]") || strings.Contains(output, `"drop"`) {
			test.Fatalf("SSE framing/drop/finish changed: calls%d %q", calls, output)
		}
	})
	for _, contentType := range []string{"text/event-stream", "application/json"} {
		test.Run(contentType, func(test *testing.T) {
			response := httptest.NewRecorder()
			calls := 0
			writer, finish := WrapResponse(response, ResponseHooks{MaxBytes: 32,
				Event:    func(string, []byte) ([]byte, bool) { calls++; return []byte(`{}`), false },
				Response: func(body []byte, status int) []byte { calls++; return []byte(`{}`) }})
			writer.Header().Set("Content-Type", contentType)
			input := `{"text":"` + strings.Repeat("large", 30) + `"}`
			if contentType == "text/event-stream" {
				input = "data: " + input + "\n\ndata: [DONE]\n\n"
			}
			for _, value := range []byte(input) {
				if _, err := writer.Write([]byte{value}); err != nil {
					test.Fatal(err)
				}
			}
			finish()
			if calls != 0 || !bytes.Equal(response.Body.Bytes(), []byte(input)) {
				test.Fatal("oversized SDK transform changed or retained unbounded bytes")
			}
		})
	}
	test.Run("compressed", func(test *testing.T) {
		response := httptest.NewRecorder()
		calls := 0
		writer, finish := WrapResponse(response, ResponseHooks{Response: func(body []byte, status int) []byte {
			calls++
			return nil
		}})
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Content-Encoding", "gzip")
		input := []byte{31, 139, 8, 0, 1, 2, 3}
		_, _ = writer.Write(input)
		finish()
		if calls != 0 || !bytes.Equal(input, response.Body.Bytes()) {
			test.Fatal("compressed response passed through a JSON transform")
		}
	})
}
