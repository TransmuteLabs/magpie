//go:build linux && cgo

package extension

import (
	"context"
	"debug/elf"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/testenv"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func nativeImageMappings(t *testing.T) map[string]bool {
	t.Helper()
	body, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		t.Fatal(err)
	}
	mappings := map[string]bool{}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 6 && strings.HasSuffix(fields[5], "/plugin.so") {
			mappings[fields[3]+":"+fields[4]] = true
		}
	}
	return mappings
}

func TestNativeImageRejectsELFAuditDependencies(t *testing.T) {
	source := filepath.Join(t.TempDir(), "fixture.c")
	if err := os.WriteFile(source, []byte("int fixture(void) { return 1; }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		flag string
		tag  elf.DynTag
	}{{"--audit", elf.DT_AUDIT}, {"--depaudit", elf.DT_DEPAUDIT}} {
		t.Run(fixture.flag, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "fixture.so")
			command := proc.CommandContext(testenv.DeadlineContext(t, 2*time.Second), "cc", "-shared", "-fPIC", "-o", filename,
				source, "-Wl,"+fixture.flag+",lib-unmodeled-auditor.so")
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("build ELF audit-resolution fixture: %v\n%s", err, output)
			}
			image, err := elf.Open(filename)
			if err != nil {
				t.Fatal(err)
			}
			defer image.Close()
			tags, err := image.DynValue(fixture.tag)
			if err != nil || len(tags) != 1 {
				t.Fatal("fixture did not declare an audit dependency:", err)
			}
			if _, err := nativeDependencyNames(image); err == nil {
				t.Fatal("ELF audit dependency was silently treated as a complete dependency closure")
			}
		})
	}
}

func TestNativeImageUnmappedOwnerDoesNotProveSystemAuthority(t *testing.T) {
	for _, owner := range []uint32{1, 1000, 65534, ^uint32(0)} {
		if nativeSystemOwner(owner) {
			t.Fatalf("unmapped or non-root owner %d was accepted as root-owned dependency authority", owner)
		}
	}
	if !nativeSystemOwner(0) {
		t.Fatal("a directly observed root-owned system file was rejected")
	}
}

func TestNativeImageHiddenSystemOwnershipFailsClosed(t *testing.T) {
	info, err := os.Stat("/")
	if err != nil {
		t.Fatal(err)
	}
	if info.Sys().(*syscall.Stat_t).Uid == 0 {
		t.Skip("requires an isolated user namespace that hides host root ownership")
	}
	distribution := nativeBuildExample(t)
	manager := NewManager(filepath.Join(t.TempDir(), "catalog"))
	defer managerReleaseClose(t, manager)
	entry, err := manager.Install(t.Context(), distribution)
	if err != nil {
		t.Fatal(err)
	}
	var result sdk.ToolResult
	if err := manager.Invoke(t.Context(), entry.Manifest.ID, sdk.MethodToolCall, sdk.Scope{},
		sdk.ToolRequest{ID: "pid", Arguments: json.RawMessage(`{}`)}, &result, nil); err != nil {
		t.Fatal("exact verified generation was unusable with unknown system ownership:", err)
	}
	var failure *sdk.Error
	if err := manager.Restart(t.Context(), entry.Manifest.ID); !errors.As(err, &failure) ||
		failure.Code != "native_restart_required" {
		t.Fatalf("hidden system ownership was accepted as cross-generation dependency proof: %v", err)
	}
	entries, err := manager.List()
	if err != nil || len(entries) != 1 || entries[0].Revision != entry.Revision {
		t.Fatal("failed opaque native replacement changed published authority:", err)
	}
}

func TestNativeImageChangedOriginDependencyRequiresRestart(t *testing.T) {
	if os.Getenv("MAGPIE_NATIVE_IMAGE_TEST") != "1" {
		t.Skip("explicit native dependency fixture opt-in required")
	}
	catalog, source, _ := nativeImageFixture(t)
	headerRoot, err := filepath.Abs("../../pkg/extension/native")
	if err != nil {
		t.Fatal(err)
	}
	build := func(arguments ...string) {
		t.Helper()
		command := proc.CommandContext(testenv.DeadlineContext(t, 2*time.Second), "cc", arguments...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("compile native dependency fixture: %v\n%s", err, output)
		}
	}
	main := filepath.Join(t.TempDir(), "main.c")
	body := `#include "abi.h"
extern int fixture_value(void);
int32_t magpie_extension_open_v1(const magpie_native_host_v1 *host, uint64_t *session) {
    *session = (uint64_t)fixture_value();
    return 0;
}
int32_t magpie_extension_send_v1(uint64_t session, const uint8_t *data, size_t length) { return 0; }
int32_t magpie_extension_close_v1(uint64_t session) { return 0; }
`
	if err := os.WriteFile(main, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	dependencySource := filepath.Join(t.TempDir(), "dependency.c")
	buildDependency := func(body string) {
		t.Helper()
		if err := os.WriteFile(dependencySource, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		build("-shared", "-fPIC", "-Wl,-soname,libfixture.so", "-o",
			filepath.Join(source, "bin", "libfixture.so"), dependencySource)
	}
	buildDependency("int fixture_value(void) { return 1; }")
	build("-shared", "-fPIC", "-I"+headerRoot, "-o", filepath.Join(source, "bin", "worker"),
		main, "-L"+filepath.Join(source, "bin"), "-lfixture", "-Wl,-rpath,$ORIGIN")
	first := catalogTestInstall(t, catalog, source)
	cache := newNativeImageCache(4)
	image, err := cache.acquire(t.Context(), catalog.Root, first, loadNativeLibrary)
	if err != nil {
		t.Fatal(err)
	}
	session, err := image.open(0)
	if err != nil || session != 1 {
		t.Fatalf("origin-relative dependency failed to load from retained snapshot: %d %v", session, err)
	}
	image.close(session)
	buildDependency("int fixture_value(void) { return 2; }")
	second := catalogTestInstall(t, catalog, source)
	if first.Files[first.Manifest.Entry] != second.Files[second.Manifest.Entry] ||
		first.Files["bin/libfixture.so"] == second.Files["bin/libfixture.so"] {
		t.Fatal("fixture must change only the C dependency while keeping the main library identical")
	}
	_, err = cache.acquire(t.Context(), catalog.Root, second, loadNativeLibrary)
	var failure *sdk.Error
	if !errors.As(err, &failure) || failure.Code != "native_restart_required" || cache.attempts != 1 {
		t.Fatalf("changed origin dependency silently reused an old image or attempted another load: %v", err)
	}
	if current, err := cache.acquire(t.Context(), catalog.Root, first, loadNativeLibrary); err != nil || current != image {
		t.Fatalf("new unprovable dependency retired the old verified generation: %v", err)
	}
	t.Log("same main binary, changed origin C dependency refused before a second native load")
}

func TestNativeImageStagedLimitKeepsPublishedGeneration(t *testing.T) {
	if os.Getenv("MAGPIE_NATIVE_IMAGE_TEST") != "1" {
		t.Skip("explicit native image admission fixture opt-in required")
	}
	if *nativeExampleDistribution == "" {
		distribution := nativeBuildExample(t)
		command := proc.CommandContext(t.Context(), os.Args[0],
			"-test.run=^TestNativeImageStagedLimitKeepsPublishedGeneration$", "-test.v",
			"-native-test-distribution="+distribution)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("native image admission child failed: %v\n%s", err, output)
		} else {
			t.Log(string(output))
		}
		return
	}
	nativeImages.limit = 1
	manager := NewManager(filepath.Join(t.TempDir(), "catalog"))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	defer func() {
		closeCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := manager.Close(closeCtx); err != nil {
			t.Error(err)
		}
	}()
	entry, err := manager.Install(ctx, *nativeExampleDistribution)
	if err != nil {
		t.Fatal(err)
	}
	held, err := manager.Acquire(ctx, entry.Manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := os.WriteFile(filepath.Join(*nativeExampleDistribution, "new-resource"), []byte("different authority"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = manager.Install(ctx, *nativeExampleDistribution)
	var failure *sdk.Error
	if !errors.As(err, &failure) || failure.Code != "native_restart_required" {
		t.Fatalf("staged image admission did not require host restart: %v", err)
	}
	entries, err := manager.List()
	if err != nil || len(entries) != 1 || entries[0].Revision != entry.Revision {
		t.Fatal("failed native image replacement changed the published package:", err)
	}
	raw, err := held.Call(ctx, sdk.MethodToolCall, sdk.Scope{},
		sdk.ToolRequest{ID: "pid", Arguments: json.RawMessage(`{"text":"old generation"}`)}, nil)
	if err != nil {
		t.Fatal("failed native replacement retired its held serving generation:", err)
	}
	var result sdk.ToolResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	var identity struct{ PID int }
	nativeToolContent(t, result, &identity)
	if identity.PID != os.Getpid() || nativeImages.attempts != 1 {
		t.Fatal("native refusal changed runtime or loaded another image")
	}
}

func TestNativeImageIdenticalReinstallationDoesNotExhaustTLS(t *testing.T) {
	if os.Getenv("MAGPIE_NATIVE_IMAGE_TEST") != "1" {
		t.Skip("explicit bounded native image stress opt-in required")
	}
	distribution := nativeBuildExample(t)
	before := nativeImageMappings(t)
	manager := NewManager(filepath.Join(t.TempDir(), "catalog"))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	defer func() {
		closeCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := manager.Close(closeCtx); err != nil {
			t.Error(err)
		}
	}()
	directories := map[string]bool{}
	revisions := map[string]bool{}
	for index := range 256 {
		entry, err := manager.Install(ctx, distribution)
		if err != nil {
			t.Fatalf("identical verified reinstall %d exhausted native loading: %v", index+1, err)
		}
		directories[entry.Directory] = true
		revisions[entry.Revision] = true
		if index == 0 {
			identity, err := nativeDependencyIdentity(filepath.Join(manager.root, entry.Directory, entry.Manifest.Entry))
			if err != nil || identity == "" {
				t.Fatalf("plain Go fixture system dependency closure is not verifiable: %v", err)
			}
		}
		var result sdk.ToolResult
		if err := manager.Invoke(ctx, entry.Manifest.ID, sdk.MethodToolCall, sdk.Scope{},
			sdk.ToolRequest{ID: "pid", Arguments: json.RawMessage(`{"text":"verified"}`)}, &result, nil); err != nil {
			t.Fatalf("reinstall %d did not retain a usable native session: %v", index+1, err)
		}
		var output struct {
			PID  int    `json:"pid"`
			Text string `json:"text"`
		}
		nativeToolContent(t, result, &output)
		if output.PID != os.Getpid() || output.Text != "verified" {
			t.Fatalf("reinstall %d changed execution runtime: %+v", index+1, output)
		}
	}
	if len(directories) != 256 || len(revisions) != 256 {
		t.Fatal("image reuse collapsed package generations")
	}
	after := nativeImageMappings(t)
	for key := range before {
		delete(after, key)
	}
	if len(after) != 1 {
		t.Fatalf("256 identical package generations mapped %d native images, want one", len(after))
	}
	t.Log("256 distinct verified package generations, 256 usable native sessions, one mapped image")
}
