package extension

import "testing"

func TestAgentLaunchEnvironmentConfigRequiresADeclaredPathOrProfile(t *testing.T) {
	manifest := agentMetadataFixture()
	manifest.Agents[0].ConfigFiles, manifest.Agents[0].Profile = nil, nil
	manifest.Agents[0].Launch.Env["CONFIG"] = "{config}"
	if err := manifest.Validate(); err == nil {
		t.Fatal("a launch environment accepted config substitution without a configuration path or profile")
	}
	for _, configuration := range []string{"file", "profile"} {
		t.Run(configuration, func(t *testing.T) {
			valid := manifest.Clone()
			if configuration == "file" {
				valid.Agents[0].ConfigFiles = []string{"config with spaces.json"}
			} else {
				valid.Agents[0].Profile = &AgentProfile{Target: "base"}
			}
			if err := valid.Validate(); err != nil {
				t.Fatal("declared config source was rejected:", err)
			}
		})
	}
}
