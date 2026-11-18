package extension

import (
	"reflect"
	"strings"
	"testing"
)

func resumeManifest(storage bool) Manifest {
	manifest := Manifest{Schema: 1, ID: "resume.fixture", Version: "1.0.0", Runtime: "native", Entry: "module.so",
		Contract: Contract{Version: APIVersion{Major: 1, Minor: 10}, Capabilities: []Capability{AgentWiring}},
		Agents: []AgentDescriptor{{ID: "vendor-sdk", Name: "Vendor", Bin: "vendor",
			Profile: &AgentProfile{Target: "vendor"}, Launch: &AgentLaunch{}}},
	}
	recipe := &SessionResumeRecipe{Agent: "vendor-sdk", Args: []string{"--resume", "{sessionId}"}}
	if storage {
		manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, StorageSource)
		manifest.StorageSources = []StorageSourceDescriptor{{ID: "history", Agent: "vendor", Codec: "vendor-sqlite",
			Roots:      []SessionSourceStore{{Slot: "home", HomeRelativePath: ".vendor"}},
			Families:   []StorageFamily{{ID: "history", Primary: "history.sqlite"}},
			Transcript: true, Resume: "agent", AgentResume: recipe}}
	} else {
		manifest.Contract.Capabilities = append(manifest.Contract.Capabilities, SessionSource)
		manifest.SessionSources = []SessionSourceDescriptor{{ID: "history", Agent: "vendor", Codec: "vendor-jsonl",
			Container: "jsonl", Stores: []SessionSourceStore{{Slot: "home", HomeRelativePath: ".vendor"}},
			Layout:     SessionSourceLayout{Kind: "flat", IDFrom: "filename-stem"},
			Transcript: true, Resume: "agent", AgentResume: recipe}}
	}
	return manifest
}

func manifestResumeRecipe(manifest *Manifest) *SessionResumeRecipe {
	if len(manifest.StorageSources) > 0 {
		return manifest.StorageSources[0].AgentResume
	}
	return manifest.SessionSources[0].AgentResume
}

func TestSessionResumeSourceContract(t *testing.T) {
	for _, storage := range []bool{false, true} {
		name := "jsonl"
		if storage {
			name = "storage"
		}
		t.Run(name, func(t *testing.T) {
			manifest := resumeManifest(storage)
			if err := manifest.Validate(); err != nil {
				t.Fatal(err)
			}
			cloned := manifest.Clone()
			manifestResumeRecipe(&cloned).Args[0] = "--changed"
			if manifestResumeRecipe(&manifest).Args[0] != "--resume" {
				t.Fatal("cloned package changed its original resume recipe")
			}
			for _, mode := range []string{"none", "native"} {
				legacy := manifest.Clone()
				legacy.Contract.Version.Minor = 8
				if storage {
					legacy.StorageSources[0].Resume, legacy.StorageSources[0].AgentResume = mode, nil
				} else {
					legacy.SessionSources[0].Resume, legacy.SessionSources[0].AgentResume = mode, nil
				}
				if err := legacy.Validate(); err != nil {
					t.Fatalf("existing %s resume changed: %v", mode, err)
				}
			}
			for name, change := range map[string]func(*Manifest){
				"old-api":         func(value *Manifest) { value.Contract.Version.Minor = 9 },
				"foreign-profile": func(value *Manifest) { value.Agents[0].Profile.Target = "another" },
				"missing-agent":   func(value *Manifest) { manifestResumeRecipe(value).Agent = "absent" },
				"no-launch":       func(value *Manifest) { value.Agents[0].Launch = nil },
				"duplicate-placeholder": func(value *Manifest) {
					manifestResumeRecipe(value).Args = []string{"{sessionId}", "{sessionId}"}
				},
				"missing-placeholder": func(value *Manifest) { manifestResumeRecipe(value).Args = []string{"--resume"} },
				"partial-placeholder": func(value *Manifest) { manifestResumeRecipe(value).Args = []string{"--resume={sessionId}"} },
				"unknown-placeholder": func(value *Manifest) { manifestResumeRecipe(value).Args = []string{"{path}", "{sessionId}"} },
				"control":             func(value *Manifest) { manifestResumeRecipe(value).Args = []string{"--resume\n", "{sessionId}"} },
				"large": func(value *Manifest) {
					manifestResumeRecipe(value).Args = []string{strings.Repeat("x", 1025), "{sessionId}"}
				},
				"undeclared-mode": func(value *Manifest) {
					if storage {
						value.StorageSources[0].Resume = "native"
					} else {
						value.SessionSources[0].Resume = "native"
					}
				},
			} {
				t.Run(name, func(t *testing.T) {
					value := manifest.Clone()
					change(&value)
					if err := value.Validate(); err == nil {
						t.Fatal("unowned or invalid session resume recipe was admitted")
					}
				})
			}
		})
	}
}

func TestSessionResumeArgumentsAreData(t *testing.T) {
	recipe := SessionResumeRecipe{Agent: "vendor-sdk", Args: []string{"--resume", "{sessionId}"}}
	id := "session with 'quotes' and ; shell"
	got, err := recipe.Arguments(id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"--resume", id}) || recipe.Args[1] != "{sessionId}" {
		t.Fatal("resume identity was interpreted or mutated", got, recipe)
	}
	for _, id := range []string{"", "--help", "../other", "nested/id", "line\nbreak", strings.Repeat("x", 129)} {
		if _, err := recipe.Arguments(id); err == nil {
			t.Fatal("invalid resume identity admitted", id)
		}
	}
}
