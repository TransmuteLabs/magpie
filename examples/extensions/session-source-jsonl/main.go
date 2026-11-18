package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"unicode/utf8"

	sdk "github.com/yetone/magpie/pkg/extension"
	"github.com/yetone/magpie/pkg/extension/native"
)

//go:embed magpie-plugin.json
var declaration []byte

var manifest sdk.Manifest

type entry struct {
	ID    string         `json:"id"`
	Cwd   string         `json:"cwd"`
	Title string         `json:"title"`
	At    string         `json:"at"`
	Role  string         `json:"role"`
	Text  string         `json:"text"`
	Input uint64         `json:"input"`
	Wait  bool           `json:"wait"`
	Blob  *blobReference `json:"blob"`
}

type blobReference struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func init() {
	if err := json.Unmarshal(declaration, &manifest); err != nil {
		panic(err)
	}
	if runtime.GOOS == "darwin" {
		manifest.Entry = "plugin.dylib"
	}
	if runtime.GOOS == "windows" {
		manifest.Entry = "plugin.dll"
	}
	if err := native.Register(sdk.Module{Manifest: manifest,
		Handlers: map[string]sdk.Handler{sdk.MethodSessionSourceRead: readSource, sdk.MethodSessionConvert: convert},
		Actions:  map[string]sdk.Handler{"session.inputs.v1": planInputs}}); err != nil {
		panic(err)
	}
}

func main() {}

func readSource(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
	request, err := sdk.DecodeSessionSourceRequest(manifest, call.Params)
	if err != nil {
		return nil, err
	}
	result := sdk.SessionSourceReadResult{Mode: request.Mode, SessionID: request.Locator.SessionID}
	summary := &sdk.SessionSourceSummary{Usage: []sdk.SessionUsagePoint{}}
	parts := []sdk.SessionSourcePart{}
	textLeft := request.Limits.MaxTextBytes
	reader := bufio.NewScanner(bytes.NewReader(request.Documents[0].Body))
	reader.Buffer(make([]byte, 4096), 16<<20)
	header := false
	var blob *blobReference
	for reader.Scan() {
		var row entry
		if err := json.Unmarshal(reader.Bytes(), &row); err != nil {
			return nil, err
		}
		if row.Wait {
			if row.Text != "" {
				if err := os.WriteFile(row.Text, []byte("native-read-started"), 0o600); err != nil {
					return nil, err
				}
			}
			<-ctx.Done()
			return nil, ctx.Err()
		}
		if row.ID != "" && row.ID != request.Locator.SessionID {
			return nil, errors.New("synthetic header does not match selected identity")
		}
		header = header || row.ID == request.Locator.SessionID
		if row.Blob != nil {
			blob = row.Blob
		}
		if row.Cwd != "" {
			summary.Cwd = row.Cwd
		}
		if row.Title != "" {
			summary.Title = fmt.Sprintf("%s pid:%d", row.Title, os.Getpid())
		}
		if row.At != "" {
			if summary.Start == "" {
				summary.Start = row.At
			}
			summary.Last = row.At
		}
		if row.Input != 0 {
			summary.Usage = append(summary.Usage, sdk.SessionUsagePoint{At: row.At, Model: "fixture-model", Input: row.Input})
		}
		if row.Text != "" {
			if len(parts) >= request.Limits.MaxParts || textLeft == 0 {
				result.Cut = true
			} else {
				text := row.Text
				cut := 0
				if len(text) > textLeft {
					text = text[:textLeft]
					for !utf8.ValidString(text) {
						text = text[:len(text)-1]
					}
					cut = utf8.RuneCountInString(row.Text) - utf8.RuneCountInString(text)
					result.Cut = true
				}
				textLeft -= len(text)
				parts = append(parts, sdk.SessionSourcePart{Role: row.Role, Kind: "text", Text: text, Cut: cut})
			}
		}
	}
	if err := reader.Err(); err != nil {
		return nil, err
	}
	if !header {
		return nil, errors.New("unsupported synthetic session format")
	}
	if blob != nil {
		found := false
		for _, document := range request.Documents[1:] {
			hash := sha256.Sum256(document.Body)
			if document.Path == blob.Path && fmt.Sprintf("%x", hash[:]) == blob.SHA256 {
				found = true
			}
		}
		if !found {
			return nil, errors.New("selected attachment was not supplied intact")
		}
	}
	if request.Mode == "summary" {
		result.Summary = summary
		result.Cut = false
	} else {
		result.Parts = parts
	}
	return result, nil
}

func convert(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
	var request sdk.SessionRequest
	if err := json.Unmarshal(call.Params, &request); err != nil {
		return nil, err
	}
	if request.ID != "to-omp" || request.From != "fixture-source" || request.To != "omp" {
		return nil, errors.New("unsupported synthetic conversion")
	}
	var options struct {
		Cwd string `json:"cwd"`
	}
	if err := json.Unmarshal(request.Options, &options); err != nil {
		return nil, err
	}
	header, _ := json.Marshal(map[string]any{"type": "session", "version": 3, "id": "converted-source", "cwd": options.Cwd, "timestamp": "2026-11-13T00:00:00Z"})
	message, _ := json.Marshal(map[string]any{"type": "message", "id": "m1", "parentId": nil, "timestamp": "2026-11-13T00:00:01Z",
		"message": map[string]any{"role": "user", "content": []map[string]string{{"type": "text", "text": "converted synthetic source"}}}})
	body := append(append(append(header, '\n'), message...), '\n')
	result := sdk.SessionResult{ID: "converted-source",
		Files: []sdk.SessionDocument{{Path: "bucket/time_converted-source.jsonl", Format: "omp", Body: body}}}
	for _, document := range request.Files[1:] {
		result.Files = append(result.Files, sdk.SessionDocument{Path: "zz-artifacts/" + document.Path, Format: "omp", Body: document.Body})
	}
	return result, ctx.Err()
}

func planInputs(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
	var request sdk.SessionRequest
	if err := json.Unmarshal(call.Params, &request); err != nil {
		return nil, err
	}
	files := []blobReference{}
	reader := bufio.NewScanner(bytes.NewReader(request.Files[0].Body))
	reader.Buffer(make([]byte, 4096), 16<<20)
	for reader.Scan() {
		var row entry
		if err := json.Unmarshal(reader.Bytes(), &row); err != nil {
			return nil, err
		}
		if row.Blob != nil {
			files = append(files, *row.Blob)
		}
	}
	return map[string]any{"version": 1, "files": files}, reader.Err()
}
