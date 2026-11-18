package settingsapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/settings"
)

// JSON deliberately exercises the existing public settings boundary, so this
// regression also compiles against the source before RequestPolicy existed.
func TestRequestPolicySettingsRoundTrip(t *testing.T) {
	home(t)
	const policy = `{"defaults":{"maxRetries":0},"providers":{"example":{"maxRetries":4}}}`
	if _, _, err := Apply([]byte(`{"requestPolicy":`+policy+`}`), false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Apply([]byte(`{"theme":"dark"}`), false); err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(settings.Load())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"requestPolicy":`+policy) {
		t.Fatalf("policy lost at ordinary settings save: %s", got)
	}
}

func TestRequestPolicySettingsRejectInvalid(t *testing.T) {
	home(t)
	for _, value := range []string{
		`{"defaults":{"maxRetries":-1}}`,
		`{"defaults":{"jitterPercent":101}}`,
		`{"defaults":{"rules":[{"status":[429],"class":"anything","scope":"account"}]}}`,
	} {
		if _, _, err := Apply([]byte(`{"requestPolicy":`+value+`}`), false); err == nil {
			t.Errorf("invalid policy accepted: %s", value)
		}
	}
}
