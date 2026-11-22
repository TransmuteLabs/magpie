package gateway

import (
	"errors"
	"strings"
	"testing"
	"testing/iotest"
)

type socketSSEProbe struct {
	strings.Reader
	first *byte
	dirty bool
}

func (probe *socketSSEProbe) Read(buf []byte) (int, error) {
	if probe.first == nil {
		probe.first = &buf[0]
		for _, value := range buf {
			if value != 0 {
				probe.dirty = true
				break
			}
		}
	}
	return probe.Reader.Read(buf)
}

func TestSocketSSEReusesCleanBuffer(test *testing.T) {
	stop := errors.New("socket write stopped")
	seen := make(map[*byte]bool)
	for iteration := range 24 {
		var probe socketSSEProbe
		probe.Reset("data: old reply bytes\n\ndata: next event\n\n")
		var wantErr error
		if iteration%2 == 0 {
			wantErr = stop
		}
		if err := readSocketSSE(&probe, func(_, _ string) error { return wantErr }); err != wantErr {
			test.Fatalf("parse returned %v, want %v", err, wantErr)
		}
		if probe.first == nil {
			test.Fatal("parser did not read the fixture")
		}
		if probe.dirty {
			test.Fatal("parser reused a buffer containing prior reply bytes")
		}
		seen[probe.first] = true
	}
	if len(seen) > 8 {
		test.Fatalf("parser used %d buffers for 24 sequential streams", len(seen))
	}
}

func TestSocketSSEKeepsFraming(test *testing.T) {
	type event struct {
		name string
		data string
	}
	large := strings.Repeat("x", 128<<10)
	tests := []struct {
		name string
		wire string
		want []event
	}{
		{
			name: "comment, CRLF, multiline and partial EOF",
			wire: ": ping\r\n\r\nevent: fragment\r\ndata: first\r\ndata:  second\r\n\r\ndata: [DONE]",
			want: []event{{name: "fragment", data: "first\n second"}, {data: "[DONE]"}},
		},
		{
			name: "bare error after data",
			wire: "data: first\n" + agBareError,
			want: []event{{data: "first"}, {data: agBareError}},
		},
		{
			name: "line grows past startup buffer",
			wire: "event: large\ndata: " + large + "\ndata: tail\n\n",
			want: []event{{name: "large", data: large + "\ntail"}},
		},
	}
	for _, fixture := range tests {
		test.Run(fixture.name, func(subtest *testing.T) {
			var got []event
			reader := iotest.HalfReader(strings.NewReader(fixture.wire))
			err := readSocketSSE(reader, func(name, data string) error {
				got = append(got, event{name: name, data: data})
				return nil
			})
			if err != nil || len(got) != len(fixture.want) {
				subtest.Fatalf("read %d events, want %d, error %v", len(got), len(fixture.want), err)
			}
			for index, want := range fixture.want {
				if got[index] != want {
					subtest.Errorf("event %d has name %q and %d bytes, want %q and %d bytes",
						index, got[index].name, len(got[index].data), want.name, len(want.data))
				}
			}
		})
	}
}

const socketSSEShort = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n" +
	"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
	"data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\"}}\n\n" +
	"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"usage\":{\"input_tokens\":20,\"output_tokens\":4}}}\n\n"

func BenchmarkSocketSSERead(bench *testing.B) {
	tests := []struct {
		name   string
		wire   string
		events int
	}{
		{name: "four_events", wire: socketSSEShort, events: 4},
		{name: "500x2KiB", wire: strings.Repeat("data: "+strings.Repeat("x", 2<<10)+"\n\n", 500), events: 500},
		{name: "128KiB_multiline", wire: "data: " + strings.Repeat("x", 128<<10) + "\ndata: tail\n\n", events: 1},
	}
	for _, fixture := range tests {
		bench.Run(fixture.name, func(sample *testing.B) {
			sample.ReportAllocs()
			sample.SetBytes(int64(len(fixture.wire)))
			reader := new(strings.Reader)
			count := 0
			accept := func(_, _ string) error {
				count++
				return nil
			}
			for sample.Loop() {
				reader.Reset(fixture.wire)
				count = 0
				if err := readSocketSSE(reader, accept); err != nil || count != fixture.events {
					sample.Fatalf("read %d events, want %d, error %v", count, fixture.events, err)
				}
			}
		})
	}
}

func BenchmarkSocketSSEReadParallel(bench *testing.B) {
	bench.ReportAllocs()
	bench.SetBytes(int64(len(socketSSEShort)))
	bench.SetParallelism(4)
	bench.RunParallel(func(worker *testing.PB) {
		reader := new(strings.Reader)
		count := 0
		accept := func(_, _ string) error {
			count++
			return nil
		}
		for worker.Next() {
			reader.Reset(socketSSEShort)
			count = 0
			if err := readSocketSSE(reader, accept); err != nil || count != 4 {
				bench.Errorf("read %d events, want 4, error %v", count, err)
				return
			}
		}
	})
}
