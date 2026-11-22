package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/tidwall/gjson"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

func TestResponsesSocketReusesRecentRequestPreview(t *testing.T) {
	fresh(t)
	t.Setenv("TMPDIR", t.TempDir())
	up := &respUp{}
	vendor := httptest.NewServer(withoutAnonymousFixtureProbe(up))
	t.Cleanup(vendor.Close)
	if err := provider.Save(provider.Provider{ID: "fake", Name: "Fake", Key: "k", Responses: vendor.URL + "/v1", Models: []string{"m1"}}); err != nil {
		t.Fatal(err)
	}
	if err := settings.Save(settings.Settings{RequestArchive: true}); err != nil {
		t.Fatal(err)
	}
	bucket := &memBucket{objs: map[string][]byte{}}
	archiveTo(t, bucket)
	server, front := sockServer(t)
	socket, _, err := dialSock(t, front, "/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}

	prompt := strings.Repeat("a ", (callBodyLimit+128)/2)
	item := strings.Replace(userHi, `"hi"`, `"`+prompt+`"`, 1)
	first := last(socket.turn(create("", item)))
	if first.Get("type").String() != "response.completed" {
		t.Fatalf("first turn: %.300s", first.Raw)
	}
	delta := last(socket.turn(create(first.Get("response.id").String(), userMore)))
	if delta.Get("type").String() != "response.completed" {
		t.Fatalf("continuation: %.300s", delta.Raw)
	}
	position := callBodyLimit - 250
	editedPrompt := prompt[:position] + "Z" + prompt[position+1:]
	edited := last(socket.turn(create("", strings.Replace(item, prompt, editedPrompt, 1))))
	if edited.Get("type").String() != "response.completed" {
		t.Fatalf("edited turn: %.300s", edited.Raw)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	server.sockets.drain(ctx)
	if ctx.Err() != nil {
		t.Fatal(ctx.Err())
	}
	archivePending.Wait()

	calls := server.Recent()
	if len(calls) != 3 || up.calls() != 3 {
		t.Fatalf("recorded %d calls, sent %d, want three each", len(calls), up.calls())
	}
	for index, call := range calls {
		if len(call.RequestBody) != callBodyLimit || !call.RequestTruncated || call.Status != http.StatusOK {
			t.Fatalf("call %d: preview %d bytes, truncated %v, status %d", index, len(call.RequestBody), call.RequestTruncated, call.Status)
		}
		var archived Archived
		if err := json.Unmarshal(bucket.objs["archive/"+call.Archive+".json"], &archived); err != nil {
			t.Fatalf("call %d archive: %v", index, err)
		}
		if !strings.HasPrefix(archived.Request.Body, call.RequestBody) || archived.Request.Truncated {
			t.Fatalf("call %d archive: %d bytes, truncated %v", index, len(archived.Request.Body), archived.Request.Truncated)
		}
		expectedPrompt := prompt
		if index == 0 {
			expectedPrompt = editedPrompt
		}
		sentInput := gjson.GetBytes(up.body(len(calls)-1-index), "input")
		archivedInput := gjson.Get(archived.Request.Body, "input")
		if sentInput.Get("0.content.0.text").String() != expectedPrompt || archivedInput.Raw != sentInput.Raw {
			t.Fatalf("call %d recorded or sent the wrong input", index)
		}
		if index == 1 && (sentInput.Get("1.content.0.text").String() != "answer 1" ||
			sentInput.Get("2.content.0.text").String() != "and more") {
			t.Fatal("continuation lost the previous answer or its new input")
		}
	}

	editedPreview, continuedPreview, firstPreview := calls[0].RequestBody, calls[1].RequestBody, calls[2].RequestBody
	if firstPreview != continuedPreview {
		t.Fatal("continuation changed the first 256 KiB of the preview")
	}
	if !strings.HasPrefix(editedPreview, firstPreview[:callBodyLimit-250]) || editedPreview == firstPreview {
		t.Fatal("edited request did not change only the end of the capped prefix")
	}
	if unsafe.StringData(editedPreview) == unsafe.StringData(firstPreview) {
		t.Error("edited request reused the old preview")
	}
	if unsafe.StringData(continuedPreview) != unsafe.StringData(firstPreview) {
		t.Error("continuation copied an identical capped preview")
	}
}
