package extension

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type ownedOutputReader struct {
	fixture *ownedFixture
	seq     uint64
}

func (reader *ownedOutputReader) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	value, err := reader.fixture.host(sdk.MethodProcessRead, sdk.ProcessReadRequest{
		Handle: reader.fixture.parent.handle, Pipe: "stdout", Seq: strconv.FormatUint(reader.seq, 10),
		MaxBytes: min(len(buffer), sdk.MaxChunkBytes),
	})
	if err != nil {
		return 0, err
	}
	reader.seq++
	result := value.(sdk.ProcessReadResult)
	count := copy(buffer, result.Bytes)
	if result.EOF {
		return count, io.EOF
	}
	return count, nil
}

func ownedPeerFixture(t *testing.T, mode string) (*ownedFixture, map[string]json.RawMessage) {
	t.Helper()
	peer := os.Getenv("OWNED_PROCESS_PEER")
	if peer == "" {
		t.Skip("source-locked peer is supplied only by the isolated replay runner")
	}
	wirePath := filepath.Join(filepath.Dir(peer), "wire.json")
	source, err := os.ReadFile(peer)
	if err != nil {
		t.Fatal(err)
	}
	wireBytes, err := os.ReadFile(wirePath)
	if err != nil {
		t.Fatal(err)
	}
	sourceSum, wireSum := sha256.Sum256(source), sha256.Sum256(wireBytes)
	fixture := newOwnedFixtureWith(t, func(launch *OwnedProcessLaunch) {
		launch.Executable = "/usr/bin/python3"
		launch.Recipe.Argv = []string{"-I", peer, "app-server", "--stdio"}
		launch.Env = []string{"ZCODE_CONTRACT_CASE=" + mode}
		launch.WatchPaths = []string{filepath.Dir(peer)}
		launch.Check = func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			currentSource, sourceErr := os.ReadFile(peer)
			currentWire, wireErr := os.ReadFile(wirePath)
			if sourceErr != nil || wireErr != nil || sha256.Sum256(currentSource) != sourceSum ||
				sha256.Sum256(currentWire) != wireSum {
				return errors.New("synthetic peer source changed")
			}
			return nil
		}
	})
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(wireBytes, &wire); err != nil {
		t.Fatal(err)
	}
	fixture.open(t)
	return fixture, wire
}

func peerFrame(t *testing.T, decoder *json.Decoder, expected json.RawMessage) {
	t.Helper()
	var actual, want any
	if err := decoder.Decode(&actual); err != nil {
		t.Fatal("owned peer could not make duplex progress", err)
	}
	if json.Unmarshal(expected, &want) != nil || !reflect.DeepEqual(actual, want) {
		t.Fatal("owned transport changed the correlated peer frame")
	}
}

func peerWrite(t *testing.T, fixture *ownedFixture, seq uint64, frame json.RawMessage) {
	t.Helper()
	var compact bytes.Buffer
	if err := json.Compact(&compact, frame); err != nil {
		t.Fatal(err)
	}
	fixture.write(t, seq, compact.String()+"\n")
}

func TestOwnedProcessPeerReverseReplyAndStopBypassPendingExecution(t *testing.T) {
	fixture, wire := ownedPeerFixture(t, "duplex")
	decoder := json.NewDecoder(&ownedOutputReader{fixture: fixture, seq: 1})
	peerWrite(t, fixture, 1, wire["subscribe"])
	peerFrame(t, decoder, wire["subscribeAck"])
	peerWrite(t, fixture, 2, wire["send"])
	peerFrame(t, decoder, wire["sendAck"])
	peerFrame(t, decoder, wire["reverse"])
	peerWrite(t, fixture, 3, wire["query"])
	var pending struct {
		Result struct {
			Results []struct {
				Turn struct {
					State string `json:"state"`
				} `json:"turn"`
			} `json:"results"`
		} `json:"result"`
	}
	if err := decoder.Decode(&pending); err != nil || len(pending.Result.Results) != 1 ||
		pending.Result.Results[0].Turn.State != "running" {
		t.Fatal("admission ACK became a terminal execution", err)
	}
	peerWrite(t, fixture, 4, wire["reverseReply"])
	peerWrite(t, fixture, 5, wire["stop"])
	peerFrame(t, decoder, wire["reverseCancelled"])
	peerFrame(t, decoder, wire["stopAck"])
	peerWrite(t, fixture, 6, wire["query"])
	peerFrame(t, decoder, wire["queryResult"])
	if _, err := fixture.host(sdk.MethodProcessStdinEOF, sdk.ProcessHandleRequest{Handle: fixture.parent.handle}); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		t.Fatal("peer failed to finish after owned stdin EOF", err)
	}
	fixture.parent.close("peer-finished")
}

func TestOwnedProcessPeerLargeFramingRetainsSeparateStderrAndEOFTail(t *testing.T) {
	fixture, _ := ownedPeerFixture(t, "framing")
	body, err := io.ReadAll(&ownedOutputReader{fixture: fixture, seq: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(body, []byte("\n \t\r\n")) || bytes.HasSuffix(body, []byte("\n")) {
		t.Fatal("transport changed blank lines or final unterminated bytes")
	}
	var frame struct {
		ID     string `json:"id"`
		Result struct {
			Text string `json:"text"`
		} `json:"result"`
	}
	if json.Unmarshal(bytes.TrimSpace(body), &frame) != nil || frame.ID != "framing-1" ||
		frame.Result.Text != "中\u2028\u2029"+string(bytes.Repeat([]byte("x"), 15*1024*1024)) {
		t.Fatal("large split UTF-8 physical output was corrupted or truncated")
	}
	stderr := fixture.read(t, "stderr", 1, sdk.MaxChunkBytes)
	if string(stderr.Bytes) != "synthetic diagnostic is not a protocol frame\n" {
		t.Fatal("protocol bytes and private stderr were merged")
	}
	fixture.parent.close("peer-finished")
}
