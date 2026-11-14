package usage

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"
)

func TestSDKExecutionEvidenceRejectsInvalidArchiveRefs(t *testing.T) {
	chunk := &rowChunk{}
	chunk.add(Row{Record: Record{Time: time.Unix(1700000000, 0),
		ResponseID: "response", UpstreamResponseID: "upstream"}}, "", 1, false)
	for _, sample := range []struct {
		name   string
		change func(*rowChunk)
	}{
		{"unknown string", func(copy *rowChunk) {
			copy.executionText[0][1] = uint32(len(copy.Strings))
		}},
		{"unknown execution", func(copy *rowChunk) {
			copy.Rows[0].Varied[rowResponseAt] = rowExecutionTag | uint32(len(copy.executionText))
		}},
		{"excess executions", func(copy *rowChunk) {
			copy.executionText = append(copy.executionText, copy.executionText[0])
		}},
	} {
		t.Run(sample.name, func(t *testing.T) {
			copy := *chunk
			copy.Rows = slices.Clone(chunk.Rows)
			copy.executionText = slices.Clone(chunk.executionText)
			sample.change(&copy)
			data, locations := copy.encode(func([]byte) bool { return true })
			if decoded := (&rowChunk{Archive: data, Locations: locations}).unpack(); decoded != nil {
				t.Fatal("an archive with invalid execution text references was accepted")
			}
		})
	}
}

func TestSDKExecutionEvidenceKeepsSharedRowShapes(t *testing.T) {
	chunk := &rowChunk{}
	var inputs []Row
	for index := range 200 {
		input := Row{Record: Record{Time: time.Unix(1700000000+int64(index), 0),
			Agent: "sdk-agent", Provider: "sdk-provider", Model: "model-a",
			RequestID: fmt.Sprint("request-", index), ResponseID: fmt.Sprint("response-", index),
			Archive: fmt.Sprint("archive-", index)}}
		if index == 100 {
			input.Model = "model-b"
		}
		if index%3 != 0 {
			input.UpstreamResponseID = fmt.Sprint("upstream-", index)
			input.ReportedCost = &ReportedCost{Amount: float64(index) / 8, Currency: "EUR"}
		}
		chunk.add(input, fmt.Sprint("message-", index), int64(index), false)
		inputs = append(inputs, input)
	}
	if len(chunk.Shapes) != 2 {
		t.Fatalf("per-execution evidence created %d row shapes for two models", len(chunk.Shapes))
	}
	for _, cached := range []*rowChunk{chunk, chunk.pack().unpack()} {
		if cached == nil {
			t.Fatal("shared execution evidence did not decode")
		}
		for index, input := range inputs {
			output := cached.row(index)
			if output.RequestID != input.RequestID || output.ResponseID != input.ResponseID ||
				output.UpstreamResponseID != input.UpstreamResponseID ||
				output.Archive != input.Archive || !reflect.DeepEqual(output.ReportedCost, input.ReportedCost) {
				t.Fatalf("row %d lost execution evidence: %+v", index, output.Record)
			}
		}
	}
}

func TestSDKExecutionEvidenceSurvivesRowCaches(t *testing.T) {
	for _, sample := range []struct {
		name string
		cost *ReportedCost
	}{
		{name: "absent"},
		{name: "zero USD", cost: &ReportedCost{Amount: 0, Currency: "USD"}},
		{name: "zero unknown unit", cost: &ReportedCost{Amount: 0}},
		{name: "measured EUR", cost: &ReportedCost{Amount: 1.25, Currency: "EUR"}},
	} {
		t.Run(sample.name, func(t *testing.T) {
			input := Row{Record: Record{
				Time: time.Now(), Agent: "sdk-agent", Provider: "sdk-provider", Model: "sdk-model",
				Status: 200, RequestID: "request-id", ResponseID: "client-response",
				UpstreamResponseID: "native-response", ReportedCost: sample.cost, OwnCost: sample.cost.USD(),
			}}
			chunk := &rowChunk{}
			chunk.add(input, "message-id", 1, false)
			for _, cached := range []*rowChunk{chunk, chunk.pack().unpack()} {
				if cached == nil {
					t.Fatal("packed evidence archive could not be restored")
				}
				output := cached.row(0)
				if output.RequestID != input.RequestID || output.ResponseID != input.ResponseID ||
					output.UpstreamResponseID != input.UpstreamResponseID {
					t.Errorf("request/client/upstream response identities changed: %+v", output.Record)
				}
				if !reflect.DeepEqual(output.ReportedCost, input.ReportedCost) {
					t.Errorf("measured amount, currency or presence changed: got %+v, want %+v", output.ReportedCost, input.ReportedCost)
				}
				if !reflect.DeepEqual(output.OwnCost, input.OwnCost) {
					t.Errorf("measured USD override changed: got %v, want %v", output.OwnCost, input.OwnCost)
				}
			}
		})
	}
}
