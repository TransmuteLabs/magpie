package extension

import "testing"

func TestNativeManifestUsesSharedCapabilitiesWithoutProcessArguments(t *testing.T) {
	manifest := Manifest{
		Schema: ManifestSchema, ID: "native-example", Version: "1.0.0",
		Runtime: "native", Entry: "plugin.dylib",
		Contract: Contract{Version: FullContract().Version, Capabilities: []Capability{OperationControl}},
	}
	if err := manifest.Validate(); err != nil {
		t.Fatal("native packaging rejected:", err)
	}
	manifest.Args = []string{"--worker"}
	if err := manifest.Validate(); err == nil {
		t.Fatal("native library accepted process arguments")
	}
	manifest.Args = nil
	public := manifest.Public()
	if public.Runtime != "" || public.Entry != "" || len(public.Args) != 0 ||
		!public.Contract.Has(OperationControl) || public.ID != manifest.ID {
		t.Fatalf("packaging changed public capabilities: %+v", public)
	}
	for _, runtime := range []string{"exec", "bun"} {
		manifest.Runtime, manifest.Args = runtime, []string{"literal argument"}
		if err := manifest.Validate(); err != nil {
			t.Fatalf("%s process arguments changed: %v", runtime, err)
		}
	}
}
