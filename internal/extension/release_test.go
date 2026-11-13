package extension

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type releaseHTTPFunc func(*http.Request) (*http.Response, error)

func (roundTrip releaseHTTPFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

type releaseZIPEntry struct {
	name string
	body []byte
	mode os.FileMode
	size uint64
	raw  bool
}

func releaseTestManifest(runtimeName, version string) sdk.Manifest {
	return sdk.Manifest{
		Schema: 1, ID: "release-fixture", Version: version, Runtime: runtimeName, Entry: "bin/worker",
		Contract: sdk.Contract{Version: sdk.FullContract().Version, Capabilities: []sdk.Capability{sdk.OperationControl}},
	}
}

func releaseTestZIP(t *testing.T, entries []releaseZIPEntry) []byte {
	t.Helper()
	var data bytes.Buffer
	writer := zip.NewWriter(&data)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		mode := entry.mode
		if mode == 0 {
			mode = 0o644
		}
		header.SetMode(mode)
		var output io.Writer
		var err error
		if entry.raw {
			header.Method = zip.Store
			header.UncompressedSize64 = entry.size
			output, err = writer.CreateRaw(header)
		} else {
			output, err = writer.CreateHeader(header)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := output.Write(entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func releaseTestPackage(t *testing.T, runtimeName, version string, extra ...releaseZIPEntry) []byte {
	t.Helper()
	manifest, err := json.Marshal(releaseTestManifest(runtimeName, version))
	if err != nil {
		t.Fatal(err)
	}
	entries := []releaseZIPEntry{
		{name: sdk.ManifestFile, body: manifest},
		{name: "bin/worker", body: []byte("never executed by preparation"), mode: 0o755},
	}
	return releaseTestZIP(t, append(entries, extra...))
}

func releasePlatformName() string {
	return "magpie-plugin_" + runtime.GOOS + "_" + runtime.GOARCH + ".zip"
}

func releaseTestResponse(request *http.Request, status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)), Request: request,
	}
}

type releaseFixture struct {
	tag       string
	assets    []releaseAsset
	archive   []byte
	checksums []byte
	requests  []string
	override  func(*http.Request) *http.Response
}

func releaseNewFixture(t *testing.T, name string, archive []byte) *releaseFixture {
	t.Helper()
	digest := sha256.Sum256(archive)
	return &releaseFixture{
		tag: "v1.2.3", archive: archive,
		assets: []releaseAsset{
			{Name: name, URL: "https://downloads.example.invalid/" + name},
			{Name: "checksums.txt", URL: "https://downloads.example.invalid/checksums.txt"},
		},
		checksums: []byte(hex.EncodeToString(digest[:]) + "  " + name + "\n"),
	}
}

func (fixture *releaseFixture) client(t *testing.T) *http.Client {
	t.Helper()
	return &http.Client{Transport: releaseHTTPFunc(func(request *http.Request) (*http.Response, error) {
		fixture.requests = append(fixture.requests, request.URL.String())
		if request.Method != http.MethodGet || request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" {
			t.Errorf("release fetch sent authorization or changed method: %s %v", request.Method, request.Header)
		}
		if fixture.override != nil {
			if response := fixture.override(request); response != nil {
				return response, nil
			}
		}
		if request.URL.Host == "api.github.com" {
			body, err := json.Marshal(githubRelease{Tag: fixture.tag, Assets: fixture.assets})
			if err != nil {
				return nil, err
			}
			return releaseTestResponse(request, http.StatusOK, body), nil
		}
		if request.URL.String() == fixture.assets[1].URL {
			return releaseTestResponse(request, http.StatusOK, fixture.checksums), nil
		}
		if request.URL.String() == fixture.assets[0].URL {
			return releaseTestResponse(request, http.StatusOK, fixture.archive), nil
		}
		return nil, errors.New("test refused an unregistered HTTP request")
	})}
}

func releasePrivateTemps(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"TMPDIR", "TEMP", "TMP"} {
		t.Setenv(name, root)
	}
	return root
}

func releaseAssertEmpty(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("release preparation leaked private staging: %v %v", entries, err)
	}
}

func TestPrepareReleasePinnedLatestAndProvenance(t *testing.T) {
	for _, source := range []string{"github:owner/repo@v1.2.3", "github:owner/repo"} {
		t.Run(source, func(t *testing.T) {
			root := releasePrivateTemps(t)
			fixture := releaseNewFixture(t, releasePlatformName(), releaseTestPackage(t, "exec", "1.2.3"))
			prepared, cleanup, err := PrepareRelease(context.Background(), fixture.client(t), source)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			expected := "https://api.github.com/repos/owner/repo/releases/latest"
			if strings.Contains(source, "@") {
				expected = "https://api.github.com/repos/owner/repo/releases/tags/v1.2.3"
			}
			digest := sha256.Sum256(fixture.archive)
			provenance := PackageSource{Reference: source, Tag: "v1.2.3", Archive: releasePlatformName(), SHA256: hex.EncodeToString(digest[:])}
			if len(fixture.requests) != 3 || fixture.requests[0] != expected || prepared.Source != provenance || prepared.Source.Validate() != nil {
				t.Fatalf("incorrect source resolution/provenance: %+v requests=%v", prepared, fixture.requests)
			}
			body, err := os.ReadFile(filepath.Join(prepared.Directory, "bin", "worker"))
			if err != nil || string(body) != "never executed by preparation" {
				t.Fatal("release worker was not staged intact", string(body), err)
			}
			if runtime.GOOS != "windows" {
				for _, directory := range []string{prepared.Directory, filepath.Dir(prepared.Directory)} {
					info, err := os.Stat(directory)
					if err != nil || info.Mode().Perm() != 0o700 {
						t.Fatal("release staging is not private", directory, err)
					}
				}
				info, _ := os.Stat(filepath.Join(prepared.Directory, "bin", "worker"))
				if info.Mode().Perm() != 0o700 {
					t.Fatal("worker executable bit was not retained privately", info.Mode())
				}
			}
			cleanup()
			cleanup()
			releaseAssertEmpty(t, root)
		})
	}
}

func TestPrepareReleaseSelectsPlatformBeforeUniversalAndRestrictsUniversal(t *testing.T) {
	for _, test := range []struct {
		name, runtime string
		platform      bool
		want          bool
	}{
		{name: "universal bun", runtime: "bun", want: true},
		{name: "universal exec", runtime: "exec"},
		{name: "platform first", runtime: "exec", platform: true, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := releasePrivateTemps(t)
			name := "magpie-plugin_any.zip"
			if test.platform {
				name = releasePlatformName()
			}
			fixture := releaseNewFixture(t, name, releaseTestPackage(t, test.runtime, "1.2.3"))
			if test.platform {
				fixture.assets = append(fixture.assets, releaseAsset{Name: "magpie-plugin_any.zip", URL: "https://unused.example.invalid/universal.zip"})
			}
			prepared, cleanup, err := PrepareRelease(context.Background(), fixture.client(t), "github:owner/repo")
			if (err == nil) != test.want {
				t.Fatal("wrong platform selection result", err)
			}
			if err == nil {
				if prepared.Source.Archive != name {
					t.Fatal("wrong archive provenance", prepared.Source)
				}
				cleanup()
			}
			releaseAssertEmpty(t, root)
		})
	}
}

func TestPrepareReleaseRejectsInvalidSourcesAssetsChecksumsAndVersions(t *testing.T) {
	for _, source := range []string{"https://github.com/owner/repo", "github:", "github:owner/repo/extra", "github:../repo",
		"github:owner/repo?query", "github:user:secret@repo", "github:owner/repo#fragment", "github:owner/repo\n",
		"github:owner/repo@bad", "github:owner/repo@v1.2.3/extra", "github:owner/repo@v1.2.3@v2.0.0"} {
		t.Run(source, func(t *testing.T) {
			fixture := releaseNewFixture(t, releasePlatformName(), nil)
			if _, _, err := PrepareRelease(context.Background(), fixture.client(t), source); err == nil || len(fixture.requests) != 0 {
				t.Fatal("invalid source reached HTTP", source, fixture.requests, err)
			}
		})
	}
	for _, test := range []struct {
		name   string
		change func(*releaseFixture)
	}{
		{"duplicate platform", func(fixture *releaseFixture) { fixture.assets = append(fixture.assets, fixture.assets[0]) }},
		{"duplicate checksums", func(fixture *releaseFixture) { fixture.assets = append(fixture.assets, fixture.assets[1]) }},
		{"missing platform", func(fixture *releaseFixture) { fixture.assets[0].Name += ".wrong" }},
		{"cpu variant only", func(fixture *releaseFixture) {
			fixture.assets[0].Name = strings.TrimSuffix(releasePlatformName(), ".zip") + "_v3.zip"
		}},
		{"missing checksums", func(fixture *releaseFixture) { fixture.assets = fixture.assets[:1] }},
		{"invalid tag", func(fixture *releaseFixture) { fixture.tag = "nightly" }},
		{"wrong pinned tag", func(fixture *releaseFixture) { fixture.tag = "v1.2.4" }},
		{"wrong manifest version", func(fixture *releaseFixture) {
			replacement := releaseNewFixture(t, releasePlatformName(), releaseTestPackage(t, "exec", "1.2.4"))
			fixture.archive, fixture.checksums = replacement.archive, replacement.checksums
		}},
		{"checksum mismatch before ZIP parsing", func(fixture *releaseFixture) { fixture.archive = []byte("not a ZIP, and not the hashed archive") }},
		{"checksum missing", func(fixture *releaseFixture) { fixture.checksums = nil }},
		{"checksum duplicate", func(fixture *releaseFixture) { fixture.checksums = append(fixture.checksums, fixture.checksums...) }},
		{"checksum malformed", func(fixture *releaseFixture) { fixture.checksums[0] = 'z' }},
		{"checksum wrong filename", func(fixture *releaseFixture) {
			fixture.checksums = bytes.ReplaceAll(fixture.checksums, []byte("  magpie"), []byte("  dir/magpie"))
		}},
		{"archive declared too large", func(fixture *releaseFixture) { fixture.assets[0].Size = catalogPackageLimit + 1 }},
		{"checksums declared too large", func(fixture *releaseFixture) { fixture.assets[1].Size = releaseMetadataLimit + 1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := releasePrivateTemps(t)
			fixture := releaseNewFixture(t, releasePlatformName(), releaseTestPackage(t, "exec", "1.2.3"))
			test.change(fixture)
			prepared, cleanup, err := PrepareRelease(context.Background(), fixture.client(t), "github:owner/repo@v1.2.3")
			if err == nil || prepared.Directory != "" || cleanup != nil {
				if cleanup != nil {
					cleanup()
				}
				t.Fatal("invalid release prepared", prepared, err)
			}
			if strings.Contains(test.name, "before ZIP") && !strings.Contains(err.Error(), "SHA-256") {
				t.Fatal("ZIP was parsed before verifying its SHA-256", err)
			}
			releaseAssertEmpty(t, root)
		})
	}
	for _, location := range []string{"http://downloads.example/a", "https://user:secret@example/a", "https://example/a#fragment", "https://example/a#",
		"https://example/%0a", "https://example/?x=%00", "https:opaque", "https://example/a\n", "https:///path"} {
		t.Run(location, func(t *testing.T) {
			fixture := releaseNewFixture(t, releasePlatformName(), nil)
			fixture.assets[0].URL = location
			if _, _, err := PrepareRelease(context.Background(), fixture.client(t), "github:owner/repo"); err == nil {
				t.Fatal("invalid release URL accepted", location)
			}
		})
	}
}

func TestPrepareReleaseRejectsUnsafeZIPAndRemovesStaging(t *testing.T) {
	for _, entry := range []releaseZIPEntry{
		{name: "../escape"}, {name: "/absolute"}, {name: `dir\escape`}, {name: "C:escape"},
		{name: "dir/../escape"}, {name: "control\x00file"}, {name: "bin/worker"},
		{name: "BIN/another"}, {name: strings.ToUpper(sdk.ManifestFile)}, {name: "link", mode: os.ModeSymlink | 0o777},
		{name: "link/", mode: os.ModeSymlink | 0o777},
		{name: "device", mode: os.ModeDevice | 0o600}, {name: "pipe", mode: os.ModeNamedPipe | 0o600},
		{name: "too-large", raw: true, size: catalogFileLimit + 1},
		{name: "bin", body: []byte("file-directory conflict")},
	} {
		t.Run(entry.name, func(t *testing.T) {
			root := releasePrivateTemps(t)
			fixture := releaseNewFixture(t, releasePlatformName(), releaseTestPackage(t, "exec", "1.2.3", entry))
			if prepared, cleanup, err := PrepareRelease(context.Background(), fixture.client(t), "github:owner/repo"); err == nil {
				cleanup()
				t.Fatal("unsafe ZIP was unpacked", prepared)
			}
			releaseAssertEmpty(t, root)
		})
	}
	t.Run("manifest must be root", func(t *testing.T) {
		root := releasePrivateTemps(t)
		data, _ := json.Marshal(releaseTestManifest("exec", "1.2.3"))
		fixture := releaseNewFixture(t, releasePlatformName(), releaseTestZIP(t, []releaseZIPEntry{{name: "wrapped/" + sdk.ManifestFile, body: data}, {name: "bin/worker"}}))
		if _, cleanup, err := PrepareRelease(context.Background(), fixture.client(t), "github:owner/repo"); err == nil {
			cleanup()
			t.Fatal("wrapped manifest was accepted")
		}
		releaseAssertEmpty(t, root)
	})
	t.Run("entry count", func(t *testing.T) {
		root := releasePrivateTemps(t)
		var entries []releaseZIPEntry
		for index := range catalogFilesLimit + 1 {
			entries = append(entries, releaseZIPEntry{name: strings.Repeat("x", index/1000+1) + "/" + string(rune(0x1000+index))})
		}
		fixture := releaseNewFixture(t, releasePlatformName(), releaseTestZIP(t, entries))
		if _, cleanup, err := PrepareRelease(context.Background(), fixture.client(t), "github:owner/repo"); err == nil {
			cleanup()
			t.Fatal("unbounded entry count accepted")
		}
		releaseAssertEmpty(t, root)
	})
	t.Run("declared total", func(t *testing.T) {
		root := releasePrivateTemps(t)
		entries := []releaseZIPEntry{}
		for index := range 5 {
			entries = append(entries, releaseZIPEntry{name: string(rune('a' + index)), size: catalogFileLimit, raw: true})
		}
		fixture := releaseNewFixture(t, releasePlatformName(), releaseTestZIP(t, entries))
		if _, cleanup, err := PrepareRelease(context.Background(), fixture.client(t), "github:owner/repo"); err == nil {
			cleanup()
			t.Fatal("unbounded uncompressed total accepted")
		}
		releaseAssertEmpty(t, root)
	})
	t.Run("lying uncompressed header", func(t *testing.T) {
		root := releasePrivateTemps(t)
		archive := releaseTestPackage(t, "exec", "1.2.3", releaseZIPEntry{name: "lying", body: bytes.Repeat([]byte("x"), 128<<10)})
		signature := []byte{'P', 'K', 1, 2}
		for offset := 0; offset+46 <= len(archive); offset++ {
			if bytes.Equal(archive[offset:offset+4], signature) && offset+51 <= len(archive) && string(archive[offset+46:offset+51]) == "lying" {
				binary.LittleEndian.PutUint32(archive[offset+24:offset+28], 1)
			}
		}
		fixture := releaseNewFixture(t, releasePlatformName(), archive)
		if _, cleanup, err := PrepareRelease(context.Background(), fixture.client(t), "github:owner/repo"); err == nil {
			cleanup()
			t.Fatal("actual decompression did not check declared limits")
		}
		releaseAssertEmpty(t, root)
	})
}

func TestReleaseChecksumExactGNUForms(t *testing.T) {
	digest := strings.Repeat("a", 64)
	for _, line := range []string{digest + "  artifact.zip\n", strings.ToUpper(digest) + " *artifact.zip\r\n"} {
		expected, err := releaseChecksum([]byte(line), "artifact.zip")
		if err != nil || hex.EncodeToString(expected) != digest {
			t.Fatal("valid GNU checksum was refused", err)
		}
	}
	for _, line := range []string{digest + " artifact.zip\n", digest + "  ./artifact.zip\n", digest + "  dir/artifact.zip\n",
		digest + "  artifact.zip \n", digest + "  artifact.zip\n" + digest + " *artifact.zip\n"} {
		if _, err := releaseChecksum([]byte(line), "artifact.zip"); err == nil {
			t.Fatal("ambiguous or inexact checksum accepted", line)
		}
	}
}

func TestReleaseCheckZIPBoundsPathsBeforeAncestry(t *testing.T) {
	for _, test := range []struct {
		name  string
		valid bool
	}{
		{"bin/worker", true},
		{strings.Repeat("a", releaseZIPMaxPathBytes), true},
		{strings.Repeat("dir/", releaseZIPMaxPathDepth-1) + "worker", true},
		{strings.Repeat("a", releaseZIPMaxPathBytes+1), false},
		{strings.Repeat("dir/", releaseZIPMaxPathDepth) + "worker", false},
		{"../outside", false},
	} {
		archive := &zip.Reader{File: []*zip.File{{FileHeader: zip.FileHeader{Name: test.name}}}}
		if err := releaseCheckZIP(archive); (err == nil) != test.valid {
			t.Fatalf("ZIP path validation for %q: valid=%v error=%v", test.name, test.valid, err)
		}
	}
}

func TestPackageSourceValidatesHistoricalPlatformsAndPin(t *testing.T) {
	source := PackageSource{Reference: "github:owner/repo@v1.2.3", Tag: "v1.2.3", Archive: "magpie-plugin_windows_amd64.zip", SHA256: strings.Repeat("a", 64)}
	for _, archive := range []string{"magpie-plugin_windows_amd64.zip", "magpie-plugin_linux_arm64.zip", "magpie-plugin_any.zip"} {
		candidate := source
		candidate.Archive = archive
		if err := candidate.Validate(); err != nil {
			t.Fatal("historical platform was refused", err)
		}
		body, _ := json.Marshal(candidate)
		var decoded PackageSource
		if json.Unmarshal(body, &decoded) != nil || !reflect.DeepEqual(decoded, candidate) {
			t.Fatal("provenance did not roundtrip")
		}
	}
	for _, change := range []func(*PackageSource){
		func(source *PackageSource) { source.Tag = "v2.0.0" },
		func(source *PackageSource) { source.Reference = "https://github.com/owner/repo" },
		func(source *PackageSource) { source.Archive = "../magpie-plugin_linux_amd64.zip" },
		func(source *PackageSource) { source.Archive = "magpie-plugin_linux_amd64_v3.zip" },
		func(source *PackageSource) { source.Archive = "magpie-plugin__amd64.zip" },
		func(source *PackageSource) { source.SHA256 = strings.Repeat("A", 64) },
	} {
		candidate := source
		change(&candidate)
		if candidate.Validate() == nil {
			t.Fatal("invalid provenance accepted", candidate)
		}
	}
}

func TestReleaseHTTPBoundsRedirectsAndClientIsolation(t *testing.T) {
	for _, test := range []string{"metadata", "checksums", "archive length", "status", "truncated"} {
		t.Run(test, func(t *testing.T) {
			root := releasePrivateTemps(t)
			fixture := releaseNewFixture(t, releasePlatformName(), releaseTestPackage(t, "exec", "1.2.3"))
			fixture.override = func(request *http.Request) *http.Response {
				switch test {
				case "metadata":
					if request.URL.Host == "api.github.com" {
						return releaseTestResponse(request, 200, bytes.Repeat([]byte(" "), releaseMetadataLimit+1))
					}
				case "checksums":
					if strings.HasSuffix(request.URL.Path, "checksums.txt") {
						response := releaseTestResponse(request, 200, bytes.Repeat([]byte(" "), releaseMetadataLimit+1))
						response.ContentLength = -1
						return response
					}
				case "archive length":
					if strings.HasSuffix(request.URL.Path, ".zip") {
						response := releaseTestResponse(request, 200, nil)
						response.ContentLength = catalogPackageLimit + 1
						return response
					}
				case "status":
					return releaseTestResponse(request, 503, []byte("not an archive"))
				case "truncated":
					response := releaseTestResponse(request, 200, []byte("{}"))
					response.ContentLength = 100
					return response
				}
				return nil
			}
			if _, cleanup, err := PrepareRelease(context.Background(), fixture.client(t), "github:owner/repo"); err == nil {
				cleanup()
				t.Fatal("invalid HTTP response accepted", test)
			}
			releaseAssertEmpty(t, root)
		})
	}
	for _, target := range []string{"http://downgrade.example/archive", "https://user:secret@example/archive", "https://example/archive#fragment", "https://example/%00"} {
		t.Run("redirect "+target, func(t *testing.T) {
			fixture := releaseNewFixture(t, releasePlatformName(), nil)
			fixture.override = func(request *http.Request) *http.Response {
				response := releaseTestResponse(request, 302, nil)
				response.Header.Set("Location", target)
				return response
			}
			if _, _, err := PrepareRelease(context.Background(), fixture.client(t), "github:owner/repo"); err == nil || len(fixture.requests) != 1 {
				t.Fatal("unsafe redirect was followed", fixture.requests, err)
			}
		})
	}
	t.Run("client jar is not used or mutated", func(t *testing.T) {
		fixture := releaseNewFixture(t, releasePlatformName(), releaseTestPackage(t, "exec", "1.2.3"))
		client := fixture.client(t)
		jar, _ := cookiejar.New(nil)
		endpoint, _ := url.Parse("https://api.github.com")
		jar.SetCookies(endpoint, []*http.Cookie{{Name: "secret", Value: "must-not-be-sent"}})
		client.Jar = jar
		prepared, cleanup, err := PrepareRelease(context.Background(), client, "github:owner/repo")
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		if prepared.Directory == "" || client.Jar != jar {
			t.Fatal("supplied client was mutated")
		}
	})
}

type releaseCancelBody struct {
	cancel context.CancelFunc
}

func (body releaseCancelBody) Read(buffer []byte) (int, error) {
	body.cancel()
	return copy(buffer, "canceled"), nil
}

func (releaseCancelBody) Close() error { return nil }

func TestPrepareReleaseCancellationRemovesStaging(t *testing.T) {
	for _, stage := range []string{"metadata", "checksums", "archive"} {
		t.Run(stage, func(t *testing.T) {
			root := releasePrivateTemps(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fixture := releaseNewFixture(t, releasePlatformName(), releaseTestPackage(t, "exec", "1.2.3"))
			fixture.override = func(request *http.Request) *http.Response {
				selected := stage == "metadata" && request.URL.Host == "api.github.com" ||
					stage == "checksums" && strings.HasSuffix(request.URL.Path, "checksums.txt") ||
					stage == "archive" && strings.HasSuffix(request.URL.Path, ".zip")
				if !selected {
					return nil
				}
				response := releaseTestResponse(request, 200, nil)
				response.Body, response.ContentLength = releaseCancelBody{cancel: cancel}, -1
				return response
			}
			prepared, cleanup, err := PrepareRelease(ctx, fixture.client(t), "github:owner/repo")
			if !errors.Is(err, context.Canceled) || prepared.Directory != "" || cleanup != nil {
				t.Fatal("canceled release was prepared", prepared, err)
			}
			releaseAssertEmpty(t, root)
		})
	}
}

func TestReleaseActualCopyLimitAndCancellation(t *testing.T) {
	for _, size := range []int{32, 33} {
		var output bytes.Buffer
		count, err := releaseCopy(context.Background(), &output, strings.NewReader(strings.Repeat("x", size)), 32)
		if size == 32 && (err != nil || count != 32) || size > 32 && err == nil || output.Len() > 32 {
			t.Fatal("actual stream exceeded its limit", count, output.Len(), err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var output bytes.Buffer
	if _, err := releaseCopy(ctx, &output, strings.NewReader("data"), 32); !errors.Is(err, context.Canceled) || output.Len() != 0 {
		t.Fatal("canceled decompression wrote data", err)
	}
}
