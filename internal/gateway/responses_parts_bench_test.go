package gateway

import (
	"fmt"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

func BenchmarkResponsesPartsFromRequest(b *testing.B) {
	for _, fixture := range []struct {
		name   string
		items  int
		scalar bool
	}{
		{name: "array_one", items: 1},
		{name: "array_128", items: 128},
		{name: "scalar_one", items: 1, scalar: true},
	} {
		b.Run(fixture.name, func(b *testing.B) {
			var input strings.Builder
			input.WriteString(`{"model":"m","input":[`)
			snippet := strings.Repeat("text supplied by the client for this turn ", 12)
			for index := range fixture.items {
				if index > 0 {
					input.WriteByte(',')
				}
				role, kind := "user", "input_text"
				if index%2 == 1 {
					role, kind = "assistant", "output_text"
				}
				if fixture.scalar {
					fmt.Fprintf(&input, `{"role":%q,"content":%q}`, role, snippet)
				} else {
					fmt.Fprintf(&input, `{"role":%q,"content":[{"type":%q,"text":%q}]}`, role, kind, snippet)
				}
			}
			input.WriteString(`]}`)
			body := []byte(input.String())
			holdBody(b.Context(), body)
			b.ReportAllocs()
			for b.Loop() {
				req, err := parse(provider.Responses, body)
				if err != nil || len(req.Messages) != fixture.items {
					b.Fatalf("parsed %d input items: %v, %v", fixture.items, req, err)
				}
			}
		})
	}
}
