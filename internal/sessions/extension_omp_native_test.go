package sessions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestSessionExtensionOMPAttachmentsNative(test *testing.T) {
	distribution := os.Getenv("MAGPIE_OMP_TEST_DIST")
	fixtures := os.Getenv("MAGPIE_OMP_TEST_FIXTURES")
	if distribution == "" || fixtures == "" {
		test.Skip("requires an explicitly built OMP native distribution and synthetic fixtures")
	}
	original, err := os.ReadFile(filepath.Join(fixtures, "omp-v3.jsonl"))
	if err != nil {
		test.Fatal(err)
	}
	image, err := os.ReadFile(filepath.Join(fixtures, "image.bin"))
	if err != nil {
		test.Fatal(err)
	}
	for _, size := range []string{"fixture", "large-document"} {
		test.Run(size, func(test *testing.T) {
			setup(test)
			test.Setenv("MAGPIE_BUN", filepath.Join(test.TempDir(), "absent-bun"))
			ctx, cancel := context.WithTimeout(test.Context(), 2*time.Minute)
			defer cancel()
			host := extension.Default()
			test.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := host.Close(ctx); err != nil {
					test.Error(err)
				}
			})
			if _, err := host.Install(ctx, distribution); err != nil {
				test.Fatal(err)
			}
			text := "blob:not-an-attachment"
			if size == "large-document" {
				text = strings.Repeat("x", 4<<20) + text
			}
			journal := bytes.ReplaceAll(original, []byte("human request"), []byte(text))
			const sessionID = "019a0000-0000-7000-8000-00000000000a"
			const filename = "2026-10-25T03-50-25-123Z_" + sessionID + ".jsonl"
			sourcePath := filepath.Join(OmpDir(), "sessions", "--work-omp--", filename)
			digest := sha256.Sum256(image)
			blobPath := "blobs/" + hex.EncodeToString(digest[:])
			for name, body := range map[string][]byte{sourcePath: journal, filepath.Join(OmpDir(), filepath.FromSlash(blobPath)): image} {
				if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
					test.Fatal(err)
				}
				if err := os.WriteFile(name, body, 0o600); err != nil {
					test.Fatal(err)
				}
			}
			output := test.TempDir()
			policy := extension.ActionPolicy{Root: output, Paths: []string{"conversation.json"}, BackupDir: filepath.Join(test.TempDir(), "backups")}
			source := Session{Agent: "omp", ID: sessionID, Path: sourcePath, Cwd: "/work/omp"}
			plan, snapshot, err := convertExtensionSource(ctx, host, "omp-sessions", "omp-to-document", source,
				"omp-conversation-v1", sdk.Scope{}, json.RawMessage(`{"output":"conversation.json"}`), policy)
			if err != nil {
				test.Fatal(err)
			}
			if len(plan.Preview().Files) != 1 || plan.SessionID() != sessionID {
				test.Fatal("native import preview omitted its converted session")
			}
			documentPath := filepath.Join(output, "conversation.json")
			if _, err := os.Stat(documentPath); !os.IsNotExist(err) {
				test.Fatal("native preview published output")
			}
			if err := snapshot.check(ctx); err != nil {
				test.Fatal(err)
			}
			if _, err := plan.Execute(ctx, plan.Preview().Revision); err != nil {
				test.Fatal(err)
			}
			document, err := os.ReadFile(documentPath)
			if err != nil {
				test.Fatal(err)
			}
			if size == "large-document" && len(document) <= 8<<20 {
				test.Fatal("fixture did not cross the previous archived-document limit")
			}
			target := test.TempDir()
			reversePolicy := extension.ActionPolicy{Root: target,
				Paths: []string{"sessions/--work-omp--/" + filename, blobPath}, BackupDir: filepath.Join(test.TempDir(), "backups")}
			reverse, err := extension.NewActions(host).SessionConvert(ctx, "omp-sessions", sdk.Scope{}, sdk.SessionRequest{
				ID: "document-to-omp", From: "omp-conversation-v1", To: "omp",
				Files:   []sdk.SessionDocument{{Path: "conversation.json", Format: "omp-conversation-v1", Body: document}},
				Options: json.RawMessage(`{"output":"sessions/--work-omp--"}`),
			}, reversePolicy)
			if err != nil {
				test.Fatal(err)
			}
			if len(reverse.Preview().Files) != 2 {
				test.Fatal("native reverse preview omitted its blob or journal")
			}
			if _, err := reverse.Execute(ctx, reverse.Preview().Revision); err != nil {
				test.Fatal(err)
			}
			for name, expected := range map[string][]byte{"sessions/--work-omp--/" + filename: journal, blobPath: image} {
				body, err := os.ReadFile(filepath.Join(target, filepath.FromSlash(name)))
				if err != nil || !bytes.Equal(body, expected) {
					test.Fatalf("native checked round trip changed %s: %v", name, err)
				}
				info, err := os.Stat(filepath.Join(target, filepath.FromSlash(name)))
				if err != nil || info.Mode().Perm() != 0o600 {
					test.Fatalf("native checked output is not private: %s %v", name, err)
				}
			}
		})
	}
}

func TestSessionExtensionOMPOrdinaryCarryOverNative(test *testing.T) {
	distribution := os.Getenv("MAGPIE_OMP_TEST_DIST")
	fixtures := os.Getenv("MAGPIE_OMP_TEST_FIXTURES")
	if distribution == "" || fixtures == "" {
		test.Skip("requires an explicitly built OMP native distribution and synthetic fixtures")
	}
	setup(test)
	home, err := os.UserHomeDir()
	if err != nil {
		test.Fatal(err)
	}
	test.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	test.Setenv("MAGPIE_BUN", filepath.Join(test.TempDir(), "absent-bun"))
	previous := agentThere
	agentThere = func(target string) bool { return target == "claude" }
	test.Cleanup(func() { agentThere = previous })
	journal, err := os.ReadFile(filepath.Join(fixtures, "omp-v3.jsonl"))
	if err != nil {
		test.Fatal(err)
	}
	image, err := os.ReadFile(filepath.Join(fixtures, "image.bin"))
	if err != nil {
		test.Fatal(err)
	}
	const sourceID = "019a0000-0000-7000-8000-00000000000a"
	filename := "2026-10-25T03-50-25-123Z_" + sourceID + ".jsonl"
	sourcePath := filepath.Join(OmpDir(), "sessions", "--work-omp--", filename)
	digest := sha256.Sum256(image)
	blob := filepath.Join(OmpDir(), "blobs", hex.EncodeToString(digest[:]))
	for name, body := range map[string][]byte{sourcePath: journal, blob: image} {
		if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
			test.Fatal(err)
		}
		if err := os.WriteFile(name, body, 0o600); err != nil {
			test.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(test.Context(), 2*time.Minute)
	defer cancel()
	host := extension.Default()
	test.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := host.Close(ctx); err != nil {
			test.Error(err)
		}
	})
	if _, err := host.Install(ctx, distribution); err != nil {
		test.Fatal(err)
	}
	Reset()
	source, exists := Find("omp", sourceID)
	if !exists || !slices.ContainsFunc(source.Carry, func(carry Carry) bool { return carry.Agent == "claude" && carry.Convert }) {
		test.Fatalf("actual OMP discovery did not offer the native converter: %+v", source)
	}
	policy := extension.ActionPolicy{Root: home, CreateUnder: ".claude/projects",
		BackupDir: filepath.Join(test.TempDir(), "backups")}
	plan, snapshot, err := convertExtensionSource(ctx, host, "omp-sessions", "omp-to-claude", source, "claude",
		sdk.Scope{}, json.RawMessage(`{"cwd":"/work/omp"}`), policy)
	if err != nil {
		test.Fatal(err)
	}
	preview := plan.Preview()
	if len(preview.Files) != 1 {
		test.Fatal("native ordinary preview did not contain a Claude journal")
	}
	if _, err := os.Stat(filepath.Join(home, filepath.FromSlash(preview.Files[0].Path))); !os.IsNotExist(err) {
		test.Fatal("native ordinary preview wrote the target session")
	}
	if err := snapshot.check(ctx); err != nil {
		test.Fatal(err)
	}
	result, err := CarryOver(source, "claude")
	if err != nil {
		test.Fatal("ordinary native CarryOver failed:", err)
	}
	if result.Agent != "claude" || !strings.HasPrefix(result.Path, filepath.Join(ClaudeDir(), "projects")+string(filepath.Separator)) ||
		!strings.Contains(result.Command, "claude --resume "+result.ID) {
		test.Fatalf("ordinary result is not native-resumable: %+v", result)
	}
	output, err := os.ReadFile(result.Path)
	if err != nil || !bytes.Contains(output, []byte("human request")) ||
		bytes.Contains(output, []byte("private reasoning")) || bytes.Contains(output, []byte("abandoned sibling")) {
		test.Fatalf("ordinary target is not the selected native context: %v", err)
	}
	encodedImage := []byte(base64.StdEncoding.EncodeToString(image))
	if bytes.Count(output, encodedImage) != 2 || !bytes.Contains(output, []byte(`"name":"Bash"`)) ||
		!bytes.Contains(output, []byte(`"type":"tool_result"`)) {
		test.Fatal("ordinary CarryOver lost native images/tool history")
	}
	Reset()
	converted, exists := Find("claude", result.ID)
	if !exists || converted.Cwd != source.Cwd || converted.ID == sourceID {
		test.Fatalf("native Claude reader did not discover the applied session: %+v", converted)
	}
	for name, expected := range map[string][]byte{sourcePath: journal, blob: image} {
		body, err := os.ReadFile(name)
		if err != nil || !bytes.Equal(body, expected) {
			test.Fatal("ordinary CarryOver modified its source:", name, err)
		}
	}
	test.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(test.TempDir(), "custom-claude"))
	if routes := extensionSessionRoutes("omp"); !slices.ContainsFunc(routes, func(route SessionExtension) bool { return route.Session.To == "claude" }) {
		test.Fatal("a native converter was not bound to the current custom store")
	}
}

func TestSessionExtensionOMPRecoveredNativeRoutes(test *testing.T) {
	distribution, fixtures := os.Getenv("MAGPIE_OMP_TEST_DIST"), os.Getenv("MAGPIE_OMP_TEST_FIXTURES")
	if distribution == "" || fixtures == "" {
		test.Skip("requires explicitly built native OMP module and synthetic fixtures")
	}
	setup(test)
	home, _ := os.UserHomeDir()
	test.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	test.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	test.Setenv("XDG_DATA_HOME", "")
	test.Setenv("MAGPIE_BUN", filepath.Join(test.TempDir(), "absent-bun"))
	previous := agentThere
	agentThere = func(target string) bool { return target == "claude" || target == "codex" || target == "omp" }
	test.Cleanup(func() { agentThere = previous })
	journal, err := os.ReadFile(filepath.Join(fixtures, "omp-v3.jsonl"))
	if err != nil {
		test.Fatal(err)
	}
	oldImage, err := os.ReadFile(filepath.Join(fixtures, "image.bin"))
	if err != nil {
		test.Fatal(err)
	}
	image := bytes.Repeat([]byte("synthetic-large-image"), 150)
	oldHash, digest := sha256.Sum256(oldImage), sha256.Sum256(image)
	journal = bytes.ReplaceAll(journal, []byte(hex.EncodeToString(oldHash[:])), []byte(hex.EncodeToString(digest[:])))
	const sourceID = "019a0000-0000-7000-8000-00000000000a"
	sourcePath := filepath.Join(OmpDir(), "sessions", "--work-omp--", "2026-10-25T03-50-25-123Z_"+sourceID+".jsonl")
	blob := filepath.Join(OmpDir(), "blobs", hex.EncodeToString(digest[:]))
	for name, body := range map[string][]byte{sourcePath: journal, blob: image, filepath.Join(CodexDir(), "config.toml"): []byte("model_provider = \"fixture-provider\"\n")} {
		if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
			test.Fatal(err)
		}
		if err := os.WriteFile(name, body, 0o600); err != nil {
			test.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(test.Context(), 3*time.Minute)
	defer cancel()
	host := extension.Default()
	test.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := host.Close(ctx); err != nil {
			test.Error(err)
		}
	})
	if _, err := host.Install(ctx, distribution); err != nil {
		test.Fatal(err)
	}
	encodedImage := []byte(base64.StdEncoding.EncodeToString(image))
	native := map[string]Session{}
	apply := func(source Session, target string) Session {
		test.Helper()
		sourceBefore, err := os.ReadFile(source.Path)
		if err != nil {
			test.Fatal(err)
		}
		routes := extensionSessionRoutes(source.Agent)
		index := slices.IndexFunc(routes, func(route SessionExtension) bool { return route.Session.To == target })
		if index < 0 {
			test.Fatalf("native route missing: %s -> %s", source.Agent, target)
		}
		route := routes[index]
		options, err := sessionConversionOptions(source, target)
		if err != nil {
			test.Fatal(err)
		}
		plan, snapshot, err := convertExtensionSource(ctx, host, route.Plugin, route.Session.ID, source, target, sdk.Scope{}, options,
			extension.ActionPolicy{Root: home, CreateUnder: route.Session.OutputDir, BackupDir: filepath.Join(test.TempDir(), "backups")})
		if err != nil {
			test.Fatal("ordinary preview failed:", source.Agent, target, err)
		}
		if len(plan.Preview().Files) != 1 {
			test.Fatal("native migration must publish only one journal")
		}
		previewPath := filepath.Join(home, filepath.FromSlash(plan.Preview().Files[0].Path))
		if _, err := os.Stat(previewPath); !os.IsNotExist(err) {
			test.Fatal("ordinary preview wrote output")
		}
		if err := snapshot.check(ctx); err != nil {
			test.Fatal(err)
		}
		result, err := CarryOver(source, target)
		if err != nil {
			test.Fatal("ordinary CarryOver failed:", source.Agent, target, err)
		}
		sourceAfter, err := os.ReadFile(source.Path)
		if err != nil || !bytes.Equal(sourceBefore, sourceAfter) {
			test.Fatal("native migration changed its selected source")
		}
		if !strings.Contains(result.Command, result.ID) {
			test.Fatal("native resume command missing session id")
		}
		body, err := os.ReadFile(result.Path)
		imageCount := bytes.Count(body, encodedImage)
		if target == "codex" {
			imageCount = 0
			for _, line := range bytes.Split(body, []byte{'\n'}) {
				var envelope struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(line, &envelope) == nil && envelope.Type == "response_item" {
					imageCount += bytes.Count(line, encodedImage)
				}
			}
		}
		if err != nil || imageCount != 2 {
			test.Fatalf("native %s attachments changed: %v", target, err)
		}
		if !bytes.Contains(body, []byte("human request")) || !bytes.Contains(body, []byte("tool failed")) ||
			bytes.Contains(body, []byte("private reasoning")) || bytes.Contains(body, []byte("abandoned sibling")) {
			test.Fatal("native selected context changed")
		}
		if target == "omp" && bytes.Contains(body, []byte("blob:sha256:")) {
			test.Fatal("ordinary native reverse wrote global blob references")
		}
		if target == "codex" {
			var header struct {
				Payload struct {
					Provider string `json:"model_provider"`
				} `json:"payload"`
			}
			first, _, _ := bytes.Cut(body, []byte{'\n'})
			if json.Unmarshal(first, &header) != nil || header.Payload.Provider != "fixture-provider" {
				test.Fatal("target Codex provider ignored")
			}
		}
		Reset()
		converted, exists := Find(target, result.ID)
		if !exists || converted.Cwd != source.Cwd {
			test.Fatalf("native %s reader cannot discover output: %+v", target, converted)
		}
		transcript, err := TranscriptOf(converted)
		if err != nil || len(transcript.Parts) == 0 {
			test.Fatalf("native %s transcript unreadable: %v", target, err)
		}
		return converted
	}
	Reset()
	source, exists := Find("omp", sourceID)
	if !exists {
		test.Fatal("native OMP source not discovered")
	}
	for _, target := range []string{"claude", "codex"} {
		native[target] = apply(source, target)
	}
	apply(native["claude"], "omp")
	packedCodex := native["codex"]
	packedCodex.Path = pack(test, packedCodex.Path, false)
	Reset()
	packedSource, exists := Find("codex", packedCodex.ID)
	if !exists || !strings.HasSuffix(packedSource.Path, ".zst") {
		test.Fatal("packed native Codex source not discovered")
	}
	apply(packedSource, "omp")
	for name, expected := range map[string][]byte{sourcePath: journal, blob: image} {
		body, err := os.ReadFile(name)
		if err != nil || !bytes.Equal(body, expected) {
			test.Fatal("migration changed source or global blob:", name, err)
		}
	}
}
