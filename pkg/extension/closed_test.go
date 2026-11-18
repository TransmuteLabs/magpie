package extension

import (
	"fmt"
	"testing"
)

func TestClosedFrameHistoryIsBoundedAndRequiresAnExactMatch(t *testing.T) {
	var recent recentFrames
	for index := range maxClosedFrames + 1 {
		recent.remember(fmt.Sprint(index), uint64(index+1), "parent")
	}
	if len(recent.byID) != maxClosedFrames || recent.consume("0", 1, "parent") {
		t.Fatal("closed frame history exceeded its bounded retention")
	}
	id, seq := fmt.Sprint(maxClosedFrames), uint64(maxClosedFrames+1)
	if recent.consume(id, seq+1, "parent") || recent.consume(id, seq, "another-parent") {
		t.Fatal("closed frame history matched a forged sequence or parent")
	}
	if !recent.consume(id, seq, "parent") || recent.consume(id, seq, "parent") {
		t.Fatal("closed frame history did not consume its exact frame once")
	}
}

func TestClosedFrameEvictionDoesNotRemoveAReusedID(t *testing.T) {
	var recent recentFrames
	recent.remember("reused", 1, "")
	if !recent.consume("reused", 1, "") {
		t.Fatal("initial closed credit was missing")
	}
	recent.remember("reused", 1, "")
	for index := range maxClosedFrames - 1 {
		recent.remember(fmt.Sprint(index), 1, "")
	}
	if !recent.consume("reused", 1, "") {
		t.Fatal("old ring entry evicted a new generation of its id")
	}
}
