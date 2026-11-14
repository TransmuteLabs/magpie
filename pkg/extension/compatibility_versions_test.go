package extension

import "testing"

func TestSDK17HostCompatibilityMatchesNegotiatedMinor(test *testing.T) {
	manifest := testManifest(OperationControl)
	manifest.Contract.Version.Minor = 7
	manifest.Compatibility = &Compatibility{HostAPI: "^1.7"}
	if err := manifest.Validate(); err != nil {
		test.Fatalf("SDK 1.7 refused its own host compatibility requirement: %v", err)
	}
	if err := CheckCompatibility(manifest); err != nil {
		test.Fatalf("installed SDK 1.7 compatibility was refused: %v", err)
	}
	legacy := manifest.Clone()
	legacy.Contract.Version.Minor = 6
	if err := legacy.Validate(); err == nil {
		test.Fatal("SDK 1.6 borrowed a host compatibility floor it did not negotiate")
	}
	legacy.Compatibility = nil
	if err := legacy.Validate(); err != nil {
		test.Fatalf("legacy control became invalid without the new requirement: %v", err)
	}
	manifest.Compatibility.HostAPI = "^1.8"
	if err := manifest.Validate(); err == nil {
		test.Fatal("SDK 1.7 borrowed a future host compatibility floor")
	}
}
