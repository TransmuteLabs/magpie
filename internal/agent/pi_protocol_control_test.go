package agent

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/provider"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func TestPiProtocolBlockAdmissionControls(t *testing.T) {
	for _, state := range []string{"absent-block", "extension-off"} {
		t.Run(state, func(t *testing.T) {
			home := syncHome(t)
			settings := `{"defaultProvider":"personal","defaultModel":"own"}`
			models := `{"providers":{"personal":{"api":"personal-api"}}}`
			if state == "extension-off" {
				settings = `{"defaultProvider":"magpie","defaultModel":"relay/glm-4.6","packages":["npm:pi-magpie@0.1.6"]}`
				models = `{"providers":{"magpie":{"baseUrl":"http://127.0.0.1:1/v1","models":[]},"personal":{"api":"personal-api"}}}`
			}
			writeFile(t, filepath.Join(home, ".pi", "agent", "settings.json"), settings)
			path := filepath.Join(home, ".pi", "agent", "models.json")
			writeFile(t, path, models)
			ordinary, err := Find("pi")
			if err != nil {
				t.Fatal(err)
			}
			if err := ordinary.Field("protocol").Set("openai-responses"); err != nil {
				t.Fatal(err)
			}
			if got := readFile(path); got != models {
				t.Fatalf("%s protocol pick rewrote unowned/off file: %s", state, got)
			}
			if piProtocol(here(home)) != "openai-responses" {
				t.Fatal("deferred protocol preference was lost")
			}
			if state == "extension-off" && len(ordinary.Field("protocol").Options(nil)) != 0 {
				t.Fatal("off Pi exposed protocol choices")
			}
		})
	}
}

func TestPiProtocolManualAPIAndInvalidChoiceControls(t *testing.T) {
	ordinary, path := piProtocolBlockFixture(t)
	if err := edit.SetJSON(path, edit.KV{Path: "providers.magpie.api", Value: "anthropic-messages"}); err != nil {
		t.Fatal(err)
	}
	if err := ordinary.Sync(); err != nil {
		t.Fatal(err)
	}
	if ordinary.Field("protocol").Get() != "anthropic-messages" || piProtocolBlockRead(t, path)["api"] != "anthropic-messages" {
		t.Fatal("sync replaced a manual provider API before explicit selection")
	}
	if err := ordinary.Apply("protocol", "openai-responses"); err != nil {
		t.Fatal(err)
	}
	if piProtocolBlockRead(t, path)["api"] != "openai-responses" {
		t.Fatal("explicit selection did not supersede manual API immediately")
	}
	before := readFile(path)
	beforeStash, err := json.Marshal(stashShared())
	if err != nil {
		t.Fatal(err)
	}
	if err := ordinary.Field("protocol").Set("unsupported-protocol"); err == nil {
		t.Fatal("invalid protocol was accepted")
	}
	afterStash, err := json.Marshal(stashShared())
	if err != nil {
		t.Fatal(err)
	}
	if readFile(path) != before || !bytes.Equal(beforeStash, afterStash) {
		t.Fatal("invalid selection changed models or stash")
	}
}

func TestPiProtocolColdReadUsesOnlyPreference(t *testing.T) {
	home := syncHome(t)
	at := here(home)
	at.cold = true
	key := piProtocolKey(at)
	stash(map[string]string{key: "openai-responses"})
	field := protocolField(at, key, "unread-cold-models.json", func() string {
		t.Fatal("cold read accessed provider file")
		return ""
	}, func(string) error {
		t.Fatal("cold read invoked writer")
		return nil
	}, func() bool {
		t.Fatal("cold read probed installation")
		return false
	})
	if field.Get() != "openai-responses" || len(field.Options(nil)) != 0 {
		t.Fatal("cold discovery lost saved preference or exposed choices")
	}
}

func TestOmpProtocolNamedProfileIsolation(t *testing.T) {
	home := syncHome(t)
	defaultAgent := omp(home)
	namedDir := filepath.Join(home, ".omp", "profiles", "work", "agent")
	namedAgent := ompAt(here(home), namedDir, func() ompProviderEntry { return ompProvider() })
	for _, ordinary := range []*Agent{defaultAgent, namedAgent} {
		if err := ordinary.Field("model").Set("magpie/relay/glm-4.6"); err != nil {
			t.Fatal(err)
		}
	}
	if err := defaultAgent.Field("protocol").Set("openai-responses"); err != nil {
		t.Fatal(err)
	}
	namedModels := filepath.Join(namedDir, "models.yml")
	before := readFile(namedModels)
	if namedAgent.Field("protocol").Get() != "" {
		t.Fatal("default protocol leaked into named profile")
	}
	if err := namedAgent.Field("protocol").Set("anthropic-messages"); err != nil {
		t.Fatal(err)
	}
	if readFile(namedModels) == before {
		t.Fatal("named protocol did not rewrite its own provider")
	}
	if defaultAgent.Field("protocol").Get() != "openai-responses" || namedAgent.Field("protocol").Get() != "anthropic-messages" {
		t.Fatal("native profile protocol preferences were not isolated")
	}
	if got, _ := edit.GetYAML(filepath.Join(home, ".omp", "agent", "models.yml"), "providers.magpie.api"); got != "openai-responses" {
		t.Fatal("named profile changed default provider API")
	}
	if got, _ := edit.GetYAML(namedModels, "providers.magpie.api"); got != "anthropic-messages" {
		t.Fatal("named profile provider API was not published")
	}
}

func TestNativeProtocolHeldSetAfterSDKAdoption(t *testing.T) {
	for _, target := range []string{"pi", "omp"} {
		t.Run(target, func(t *testing.T) {
			descriptor := launchAgentDescriptor()
			descriptor.Profile = &sdk.AgentProfile{Target: target}
			descriptor.ConfigFiles = []string{".fixture/config.json"}
			descriptor.Fields = []sdk.AgentField{{Key: "model", Label: "Model"}}
			descriptor.Launch.Model = "--model"
			host, home := launchAgentFixture(t, descriptor)
			noKeychain(t)
			if err := provider.Save(provider.Provider{ID: "fixture", Name: "Fixture", Key: "synthetic", Chat: "http://127.0.0.1:1/v1", Models: []string{"model"}}); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(home, ".fixture", "config.json"), `{"model":"own"}`)
			var native *Agent
			var models string
			if target == "pi" {
				native = pi(home)
				models = filepath.Join(home, ".pi", "agent", "models.json")
			} else {
				native = omp(home)
				models = filepath.Join(home, ".omp", "agent", "models.yml")
			}
			if err := native.Field("model").Set("magpie/fixture/model"); err != nil {
				t.Fatal(err)
			}
			readModels := func() []byte {
				t.Helper()
				payload, err := os.ReadFile(models)
				if err != nil {
					t.Fatal(err)
				}
				return payload
			}
			readModels()
			held, err := Find(target)
			if err != nil || held.Field("protocol") == nil {
				t.Fatal("native protocol absent before adoption", err)
			}
			set := held.Field("protocol").Set
			profile, err := Find(descriptor.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := profile.ConnectHow(); err != nil {
				t.Fatal(err)
			}
			for _, phase := range []string{"adopted", "disabled", "replaced"} {
				if phase == "disabled" {
					if err := host.SetEnabled(t.Context(), "fixture.agent", false); err != nil {
						t.Fatal(err)
					}
				}
				if phase == "replaced" {
					if err := host.SetEnabled(t.Context(), "fixture.agent", true); err != nil {
						t.Fatal(err)
					}
					replaceAgentFixture(t, host, descriptor, "1.0.1", 5)
				}
				active, err := Find(target)
				if err != nil || active.ID != descriptor.ID || active.Field("protocol") != nil {
					t.Fatalf("%s inherited an undeclared native protocol field: %+v %v", phase, active, err)
				}
				before := readModels()
				stashBefore, err := json.Marshal(stashShared())
				if err != nil {
					t.Fatal(err)
				}
				if err := set("openai-responses"); err == nil {
					t.Fatalf("%s held native setter bypassed SDK ownership", phase)
				}
				if err := held.Apply("protocol", "anthropic-messages"); err == nil {
					t.Fatalf("%s held Apply bypassed SDK ownership", phase)
				}
				if held.Sync == nil || held.Sync() == nil {
					t.Fatalf("%s held Sync bypassed SDK ownership", phase)
				}
				stashAfter, err := json.Marshal(stashShared())
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(readModels(), before) || !bytes.Equal(stashBefore, stashAfter) {
					t.Fatalf("%s refused native mutation changed models/stash", phase)
				}
			}
			active, err := Find(target)
			if err != nil {
				t.Fatal(err)
			}
			if err := active.Disconnect(); err != nil {
				t.Fatal("profile recovery failed", err)
			}
			released, err := Find(target)
			if err != nil || released.ID != target || released.Field("protocol") == nil {
				t.Fatal("disconnect did not restore ordinary native protocol access", err)
			}
			if err := released.Field("protocol").Set("openai-responses"); err != nil {
				t.Fatal("released native setter remained fenced", err)
			}
			if err := released.Field("model").Set("magpie/fixture/model"); err != nil {
				t.Fatal("released native reconnect remained fenced", err)
			}
			if err := released.Field("protocol").Set("anthropic-messages"); err != nil {
				t.Fatal("reconnected native setter remained fenced", err)
			}
			if target == "pi" {
				if piProtocolBlockRead(t, models)["api"] != "anthropic-messages" {
					t.Fatal("released Pi setter did not publish")
				}
			} else if got, _ := edit.GetYAML(models, "providers.magpie.api"); got != "anthropic-messages" {
				t.Fatal("released OMP setter did not publish")
			}
		})
	}
}
