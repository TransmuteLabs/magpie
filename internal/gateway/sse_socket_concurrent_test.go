package gateway

import (
	"context"
	"strings"
	"testing"
	"time"
)

type socketSSEGrowthProbe struct {
	strings.Reader
	start *byte
	upper *byte
}

func (probe *socketSSEGrowthProbe) Read(destination []byte) (int, error) {
	switch probe.Size() - int64(probe.Len()) {
	case 0:
		probe.start = &destination[0]
	case bareReadSize:
		probe.upper = &destination[0]
	}
	return probe.Reader.Read(destination)
}

func TestSocketSSEConcurrentBufferBounds(test *testing.T) {
	const streams = 16
	ctx, cancel := context.WithTimeout(test.Context(), 20*time.Second)
	type lease struct {
		start, upper *byte
		dataSize     int
	}
	reading := make(chan lease, streams)
	returned := make(chan error, streams)
	release := make(chan struct{})
	finished := 0
	defer func() {
		cancel()
		if finished == streams {
			return
		}
		timeout := time.NewTimer(5 * time.Second)
		defer timeout.Stop()
		for finished < streams {
			select {
			case <-returned:
				finished++
			case <-socketSSEBuffers:
			case <-timeout.C:
				test.Errorf("%d SSE readers did not stop", streams-finished)
				return
			}
		}
	}()

	payload := strings.Repeat("x", 128<<10)
	wire := "data: " + payload + "\n\n"
	for range streams {
		go func() {
			var probe socketSSEGrowthProbe
			probe.Reset(wire)
			returned <- readSocketSSE(&probe, func(_, data string) error {
				reading <- lease{start: probe.start, upper: probe.upper, dataSize: len(data)}
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
		}()
	}

	active := make(map[*byte]lease)
	for range streams {
		select {
		case current := <-reading:
			if current.start == nil || current.upper == nil || current.dataSize != len(payload) {
				test.Fatalf("missing initial reads or truncated large event: %+v", current)
			}
			if _, reused := active[current.start]; reused {
				test.Fatal("two active SSE readers share an initial buffer")
			}
			active[current.start] = current
		case err := <-returned:
			finished++
			test.Fatalf("SSE reader returned before release: %v", err)
		case <-ctx.Done():
			test.Fatalf("only %d readers in flight: %v", len(active), ctx.Err())
		}
	}
	if spares := len(socketSSEBuffers); spares != 0 {
		test.Errorf("%d spare buffers while all readers hold their leases", spares)
	}

	close(release)
	for finished < streams {
		select {
		case err := <-returned:
			finished++
			if err != nil {
				test.Errorf("SSE reader returned %v", err)
			}
		case <-ctx.Done():
			test.Fatalf("only %d readers returned: %v", finished, ctx.Err())
		}
	}
	spares := len(socketSSEBuffers)
	if spares != 8 {
		test.Errorf("retained %d spare buffers after %d streams, want 8", spares, streams)
	}
	for range spares {
		buffer := <-socketSSEBuffers
		current, ok := active[&buffer[0]]
		if !ok || current.upper != &buffer[bareReadSize] {
			test.Error("spare was not the original buffer that received both halves of the large event")
		}
		delete(active, &buffer[0])
		for offset, value := range buffer {
			if value != 0 {
				test.Errorf("spare still contains reply data at byte %d", offset)
				break
			}
		}
	}
}
