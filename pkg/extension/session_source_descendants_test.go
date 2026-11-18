package extension

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func descendantSourceRequest() (Manifest, SessionSourceReadRequest) {
	manifest := sourceManifest()
	manifest.Contract.Version.Minor = 9
	manifest.SessionSources[0].Descendants = &SessionSourceDescendants{
		Kind: "journal-stem-jsonl", ExcludeDirectories: []string{"btw-history"},
	}
	return manifest, SessionSourceReadRequest{
		SourceID: "history", Locator: SessionSourceLocator{StoreRef: "store", Path: "sess.jsonl", SessionID: "sess"},
		Snapshot: strings.Repeat("a", 64), Mode: "summary",
		Documents: []SessionDocument{
			{Path: "sess.jsonl", Format: "fixture-jsonl-v1", Body: []byte("{}\n")},
			{Path: "sess/child/advisor.jsonl", Format: "fixture-jsonl-v1", Body: []byte("{}\n")},
		},
		Descendants: []string{"sess/child/advisor.jsonl"},
		Limits:      SessionSourceLimits{MaxParts: 100, MaxTextBytes: 400000},
	}
}

func TestSessionSourceDescendantsRequireDeclaredBoundedJournalTree(t *testing.T) {
	manifest, request := descendantSourceRequest()
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSessionSourceRequest(manifest, request); err != nil {
		t.Fatal(err)
	}
	cloned := manifest.Clone()
	cloned.SessionSources[0].Descendants.ExcludeDirectories[0] = "changed"
	if manifest.SessionSources[0].Descendants.ExcludeDirectories[0] != "btw-history" {
		t.Fatal("source descendant rules share mutable manifest authority")
	}
	for _, change := range []string{"old-api", "undeclared", "unknown-kind", "exclude-path", "exclude-glob", "duplicate-exclude", "directory-layout",
		"missing-document", "outside", "prefix", "excluded", "duplicate", "main"} {
		t.Run(change, func(t *testing.T) {
			value, input := descendantSourceRequest()
			switch change {
			case "old-api":
				value.Contract.Version.Minor = 8
			case "undeclared":
				value.SessionSources[0].Descendants = nil
			case "unknown-kind":
				value.SessionSources[0].Descendants.Kind = "recursive"
			case "exclude-path":
				value.SessionSources[0].Descendants.ExcludeDirectories = []string{"a/b"}
			case "exclude-glob":
				value.SessionSources[0].Descendants.ExcludeDirectories = []string{"*"}
			case "duplicate-exclude":
				value.SessionSources[0].Descendants.ExcludeDirectories = []string{"x", "x"}
			case "directory-layout":
				value.SessionSources[0].Layout = SessionSourceLayout{Kind: "directory", IDFrom: "parent-directory", Entry: "log.jsonl"}
			case "missing-document":
				input.Documents = input.Documents[:1]
			case "outside", "prefix", "excluded", "main":
				name := map[string]string{"outside": "other/child.jsonl", "prefix": "sess-other/child.jsonl",
					"excluded": "sess/nested/btw-history/child.jsonl", "main": "sess.jsonl"}[change]
				input.Documents[1].Path, input.Descendants[0] = name, name
			case "duplicate":
				input.Descendants = append(input.Descendants, input.Descendants[0])
			}
			if value.Validate() == nil && ValidateSessionSourceRequest(value, input) == nil {
				t.Fatal("invalid source descendant authority accepted")
			}
		})
	}
}

func TestSessionSourceDescendantIdentitiesBindExactAdmittedDocuments(t *testing.T) {
	_, request := descendantSourceRequest()
	summary := SessionSourceSummary{Title: "main", Cwd: "/work",
		Subs: []SessionSourceIdentity{{Path: request.Descendants[0], SessionID: "child-id"}}}
	body, _ := json.Marshal(SessionSourceReadResult{Mode: "summary", SessionID: "sess", Summary: &summary})
	result, err := DecodeSessionSourceResult(request, body)
	if err != nil || !reflect.DeepEqual(result.Summary.Subs, summary.Subs) {
		t.Fatal("admitted child identities were lost", result, err)
	}
	for _, change := range []string{"unadmitted", "missing", "alias-main", "duplicate", "bad-id", "borrow-attachment"} {
		t.Run(change, func(t *testing.T) {
			_, input := descendantSourceRequest()
			copy := summary
			copy.Subs = append([]SessionSourceIdentity(nil), summary.Subs...)
			switch change {
			case "unadmitted":
				input.Descendants = nil
			case "missing":
				copy.Subs = nil
			case "alias-main":
				copy.Subs[0].SessionID = "sess"
			case "duplicate":
				copy.Subs = append(copy.Subs, copy.Subs[0])
			case "bad-id":
				copy.Subs[0].SessionID = "../other"
			case "borrow-attachment":
				copy.Subs[0].Path = "attachment.jsonl"
			}
			raw, _ := json.Marshal(SessionSourceReadResult{Mode: "summary", SessionID: "sess", Summary: &copy})
			if _, err := DecodeSessionSourceResult(input, raw); err == nil {
				t.Fatal("unadmitted child identity was accepted")
			}
		})
	}
}
