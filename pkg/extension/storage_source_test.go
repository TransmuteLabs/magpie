package extension

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func storageFixtureManifest() Manifest {
	return Manifest{Schema: 1, ID: "storage-fixture", Version: "1.0.0", Runtime: "native", Entry: "module.so",
		Contract: Contract{Version: APIVersion{Major: 1, Minor: 8}, Capabilities: []Capability{StorageSource}},
		StorageSources: []StorageSourceDescriptor{{ID: "history", Agent: "fixture", Codec: "fixture-db",
			Roots: []SessionSourceStore{{Slot: "data", HomeRelativePath: ".fixture/data"}},
			Families: []StorageFamily{{ID: "db", Primary: "fixture.db", Sidecars: []string{"fixture.db-wal", "fixture.db-shm"}},
				{ID: "legacy", Trees: []string{"storage/session", "storage/message"}}}, Transcript: true, Resume: "none"}}}
}

func TestStorageNativeManifestNegotiatesOrdinaryHostContract(t *testing.T) {
	manifest := storageFixtureManifest()
	if err := manifest.Validate(); err != nil {
		t.Fatal("ordinary host rejected its declared storage capability", err)
	}
	if !slices.Equal(RequiredMethods(manifest), []string{MethodStorageList, MethodStorageRead}) {
		t.Fatal("storage source did not require both bounded reader methods")
	}
	manifest.Contract.Version.Minor = 7
	if manifest.Validate() == nil {
		t.Fatal("storage source negotiated below API 1.8")
	}
}

func TestStorageDescriptorCannotBroadenRootsOrBorrowOldContract(t *testing.T) {
	for _, mutate := range []func(*Manifest){
		func(manifest *Manifest) { manifest.Contract.Version.Minor = 7 },
		func(manifest *Manifest) { manifest.Runtime = "bun" },
		func(manifest *Manifest) { manifest.StorageSources[0].Roots[0].HomeRelativePath = "." },
		func(manifest *Manifest) { manifest.StorageSources[0].Roots[0].HomeRelativePath = "../escape" },
		func(manifest *Manifest) { manifest.StorageSources[0].Families[0].Primary = "../escape.db" },
		func(manifest *Manifest) {
			manifest.StorageSources[0].Families[0].Sidecars = []string{"db-wal", "db-wal"}
		},
	} {
		manifest := storageFixtureManifest()
		if err := ValidateStorageSources(manifest); err != nil {
			t.Fatal("positive finite storage declaration failed", err)
		}
		mutate(&manifest)
		if ValidateStorageSources(manifest) == nil {
			t.Fatal("invalid storage declaration widened its root or version")
		}
	}
	if FullContract().Version.Minor == 7 && FullContract().Check(storageFixtureManifest().Contract) == nil {
		t.Fatal("uncoordinated minor8 storage admission bypassed FullContract1.7")
	}
}

func TestStorageSelectedSessionCannotBorrowAccountOrRoot(t *testing.T) {
	manifest := storageFixtureManifest()
	request := StorageRequest{Lease: StorageReadLease{SourceID: "history", RootRef: "root", SnapshotRef: "snapshot",
		FamilyID: "db", Limits: DefaultStorageLimits()}, Mode: "transcript",
		Locator: &StorageSessionLocator{RootRef: "root", SessionID: "ses_a", Key: "v2:ses_a"}}
	scope := Scope{Session: "ses_a"}
	if err := ValidateStorageRequest(manifest, MethodStorageRead, scope, nil, request); err != nil {
		t.Fatal(err)
	}
	for _, foreign := range []Scope{{Session: "ses_b"}, {Session: "ses_a", Provider: "vendor"},
		{Session: "ses_a", Account: "key"}, {Session: "ses_a", Generation: 1}, {Session: "ses_a", Attempt: 1}} {
		if ValidateStorageRequest(manifest, MethodStorageRead, foreign, nil, request) == nil {
			t.Fatal("foreign storage authority was admitted", foreign)
		}
	}
	if ValidateStorageRequest(manifest, MethodStorageRead, scope, json.RawMessage(`{"key":"secret"}`), request) == nil {
		t.Fatal("storage source accepted credentials")
	}
	request.Locator.RootRef = "foreign"
	if ValidateStorageRequest(manifest, MethodStorageRead, scope, nil, request) == nil {
		t.Fatal("session locator escaped its root")
	}
}

func TestStorageResultRejectsForeignMediaAndSnapshot(t *testing.T) {
	request := StorageRequest{Lease: StorageReadLease{SnapshotRef: "snapshot", Limits: DefaultStorageLimits()},
		Locator: &StorageSessionLocator{SessionID: "ses_a"}, Mode: "transcript"}
	result := StorageResult{SnapshotRef: "snapshot", SessionID: "ses_a", Mode: "transcript",
		Parts: []SessionSourcePart{{Role: "user", Kind: "text", Text: "hello"}},
		Media: []StorageMedia{{ID: "media:one", MIME: "image/png", Bytes: []byte("png")}}}
	if err := ValidateStorageResult(request, result); err != nil {
		t.Fatal(err)
	}
	result.Media = append(result.Media, result.Media[0])
	if ValidateStorageResult(request, result) == nil {
		t.Fatal("duplicate media escaped the source budget")
	}
	result.Media = nil
	result.SnapshotRef = strings.Repeat("x", 32)
	if ValidateStorageResult(request, result) == nil {
		t.Fatal("foreign snapshot was published")
	}
}
