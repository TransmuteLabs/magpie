package sessions

import (
	"strings"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestSourceResumeIdentityBindsSelectedInputAndRecipe(t *testing.T) {
	makeRecord := func() *sessionSourceRecord {
		return &sessionSourceRecord{
			grant: sourceGrant{plugin: "fixture", identity: strings.Repeat("a", 64), reference: strings.Repeat("b", 64),
				descriptor: sdk.SessionSourceDescriptor{ID: "history", Agent: "fixture-agent", Resume: "agent",
					AgentResume: &sdk.SessionResumeRecipe{Agent: "fixture-launch", Args: []string{"resume", "{sessionId}"}}}},
			request: sdk.SessionSourceReadRequest{SourceID: "history", Snapshot: strings.Repeat("c", 64),
				Locator: sdk.SessionSourceLocator{StoreRef: strings.Repeat("b", 64), Path: "session.jsonl", SessionID: "session"}},
			summary: &state{Cwd: "/work/selected"},
		}
	}
	token := sourceResumeToken(makeRecord())
	if len(token) != 64 || sourceResumeToken(makeRecord()) != token {
		t.Fatal("source command lacks a stable selection identity", token)
	}
	for _, change := range []string{"snapshot", "path", "cwd", "args", "launch-agent", "resume-mode", "storage-key", "storage-root"} {
		t.Run(change, func(t *testing.T) {
			record := makeRecord()
			previous := token
			if strings.HasPrefix(change, "storage-") {
				record.storage = &storageSourceRecord{locator: sdk.StorageSessionLocator{RootRef: "root", SessionID: "session", Key: "selected"}}
				previous = sourceResumeToken(record)
			}
			switch change {
			case "snapshot":
				record.request.Snapshot = strings.Repeat("d", 64)
			case "path":
				record.request.Locator.Path = "other/session.jsonl"
			case "cwd":
				record.summary.Cwd = "/work/other"
			case "args":
				record.grant.descriptor.AgentResume.Args[0] = "open"
			case "launch-agent":
				record.grant.descriptor.AgentResume.Agent = "other-launch"
			case "resume-mode":
				record.grant.descriptor.Resume = "none"
			case "storage-key":
				record.storage.locator.Key = "other"
			case "storage-root":
				record.storage.locator.RootRef = "other"
			}
			if sourceResumeToken(record) == previous {
				t.Fatal("displayed resume identity ignored changed selection", change)
			}
		})
	}
}

func TestSourceResumeCommandQuotesFiniteArguments(t *testing.T) {
	for _, windows := range []bool{false, true} {
		line := sourceResumeLine("/tmp/Mag pie's", "fixture-agent", "session's ; $text", strings.Repeat("a", 64), windows)
		quote := shellQuote
		head := ""
		if windows {
			quote = func(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
			head = "& "
		}
		want := head + quote("/tmp/Mag pie's") + " sessions resume " + quote("fixture-agent") + " " +
			quote("session's ; $text") + " --expected " + quote(strings.Repeat("a", 64))
		if line != want {
			t.Fatal("source command lost argv quoting", windows, line)
		}
	}
	if sourceResumeLine("", "agent", "session", "token", false) != "" ||
		sourceResumeLine("magpie", "agent", "session", "", false) != "" {
		t.Fatal("resume command survived a missing executable or identity")
	}
}

func TestSourceNativeResumeUsesLinkedRecipe(t *testing.T) {
	for _, agent := range []string{"pi", "opencode", "qoder-cn"} {
		t.Run(agent, func(t *testing.T) {
			previous, registered := linkedSessionResumeRecipes[agent]
			if !registered {
				t.Fatal("agent has no linked resume recipe")
			}
			t.Cleanup(func() { linkedSessionResumeRecipes[agent] = previous })
			linkedSessionResumeRecipes[agent] = sdk.SessionResumeRecipe{
				Agent: "linked-fixture", Args: []string{"--linked-fixture", "{sessionId}"},
			}
			session := Session{
				Agent: agent, ID: "listed-id",
				source: &sessionSourceRecord{
					grant: sourceGrant{descriptor: sdk.SessionSourceDescriptor{Agent: agent, Resume: "native"}},
					request: sdk.SessionSourceReadRequest{
						Locator: sdk.SessionSourceLocator{SessionID: "source-id", Path: "source-id.jsonl"},
					},
					summary: &state{},
				},
			}
			listed(&session)
			if session.Resume != "linked-fixture --linked-fixture source-id" {
				t.Fatalf("source-backed resume = %q", session.Resume)
			}

			t.Cleanup(ForceCLIGone(agent))
			listed(&session)
			if session.Resume != "" {
				t.Fatalf("source-backed resume offered with executable gone: %q", session.Resume)
			}
		})
	}
}
