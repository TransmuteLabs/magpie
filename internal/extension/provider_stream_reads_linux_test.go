package extension

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/proc"
	sdk "github.com/yetone/magpie/pkg/extension"
	"golang.org/x/sys/unix"
)

func TestProviderStreamStoreReads(t *testing.T) {
	t.Setenv("MAGPIE_NO_ACCOUNTS", "0")
	manager, entry := fixtureManagerAt(t, 9)
	ctx := t.Context()
	scope := sdk.Scope{Plugin: entry.Manifest.ID, Provider: "provider", Account: "selected", Generation: 1}
	if _, err := manager.CompareAndSwapModuleConfig(ctx, scope.Plugin, "0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	const ownerRevision = "fixture-owner"
	if err := manager.Store().withStoreContext(ctx, savedSignInsDir, func(root *os.Root) error {
		rows := []subscriptionRecord{{
			SubscriptionOwner: SubscriptionOwner{
				Module: scope.Plugin, Provider: scope.Provider, PublicID: "fixture-provider", Revision: ownerRevision, Active: true,
				Accounts: []SubscriptionAccount{{Key: "fixture-key", ID: scope.Account, User: "fixture-user", LoginID: "fixture-login"}},
			},
			GrantRevision: strings.Repeat("a", 64),
			Digests:       map[string]string{"fixture-key": strings.Repeat("b", 64)},
			Tickets:       []string{"fixture-ticket"},
		}}
		return storeWrite(root, subscriptionOwnersFile, rows, storeFileLimit)
	}); err != nil {
		t.Fatal(err)
	}
	ctx = WithSubscriptionOwnerRevision(ctx, ownerRevision)
	ctx, _ = providerStreamSelection(t, ctx, manager, scope.Plugin)
	namespace := manager.Namespace()
	private := filepath.Join(namespace, "private")
	accountsFile := filepath.Join(private, scope.Plugin, "accounts.json")
	files := []string{
		accountsFile,
		filepath.Join(namespace, "catalog.json"),
		filepath.Join(private, scope.Plugin, "state", storeStateName(sdk.Scope{Plugin: scope.Plugin}, moduleConfigKey)),
		filepath.Join(private, savedSignInsDir, subscriptionOwnersFile),
	}
	for _, accounts := range []int{1, 10, 100} {
		for _, size := range []string{"compact", "oversize"} {
			t.Run(fmt.Sprintf("%d-accounts/%s", accounts, size), func(t *testing.T) {
				fileBytes := seedProviderStreamAccounts(t, manager, scope, accounts, size == "oversize", accountsFile)
				call := func(chunks int) {
					t.Helper()
					callscope := scope
					callscope.Operation = fmt.Sprintf("measure-%d-%s-%d", accounts, size, chunks)
					callscope.Attempt = 1
					events := 0
					_, err := manager.Call(ctx, scope.Plugin, sdk.MethodProviderExecute, callscope, providerStreamRequest(chunks), func(event Event) error {
						index := events
						events++
						switch index {
						case 0:
							if event.Name == "accepted" {
								return nil
							}
						case 1:
							if event.Name == "headers" {
								return nil
							}
						default:
							if event.Name == "data" && string(event.Data) == string(providerStreamChunk(index-2)) {
								return nil
							}
						}
						return fmt.Errorf("unexpected %q event at index %d", event.Name, index)
					})
					if err != nil || events != chunks+2 {
						t.Fatalf("execute delivered %d events, want %d: %v", events, chunks+2, err)
					}
				}
				call(1)
				call(2)
				short := countProviderStreamReads(t, files, func() { call(0) })
				long := countProviderStreamReads(t, files, func() { call(500) })
				t.Logf("accounts_bytes=%d files=[accounts,catalog,config,owners] chunks_0=%v chunks_500=%v", fileBytes, short, long)
				if short[0] > 9 {
					t.Fatalf("execute preflight exceeded nine account reads: %v", short)
				}
				if long[0] < short[0] || long[0]-short[0] > 500 {
					t.Fatalf("account reads exceeded one per event: %v to %v", short, long)
				}
				if !slices.Equal(short[1:], long[1:]) {
					t.Fatalf("catalog, config or owner reads grew with events: %v to %v", short, long)
				}
				journal, err := manager.Store().Operation(scope.Plugin, fmt.Sprintf("measure-%d-%s-500", accounts, size))
				if err != nil || journal.State != "completed" {
					t.Fatalf("stream journal did not complete: %v, %v", journal.State, err)
				}
			})
		}
	}
}

func seedProviderStreamAccounts(t *testing.T, manager *Manager, scope sdk.Scope, count int, oversize bool, path string) int64 {
	t.Helper()
	records := storeAccounts{Schema: 1, Accounts: make([]storeAccount, count)}
	for index := range records.Accounts {
		records.Accounts[index].Account = Account{
			Plugin: scope.Plugin, Provider: scope.Provider, ID: fmt.Sprintf("fixture-%03d", index),
			Generation: scope.Generation, Auth: json.RawMessage(`{}`),
		}
	}
	records.Accounts[count-1].ID = scope.Account
	records.Accounts[count-1].Auth = json.RawMessage(`{"streamFixture":true}`)
	upgradeWindowAuthStore(&records)
	if oversize {
		auth := `{"padding":"`
		if count == 1 {
			auth = `{"streamFixture":true,"padding":"`
		}
		records.Accounts[0].Auth = json.RawMessage(auth + `"}`)
		encoded, err := json.Marshal(records)
		if err != nil {
			t.Fatal(err)
		}
		records.Accounts[0].Auth = json.RawMessage(auth + strings.Repeat("x", storeSnapshotLimit-len(encoded)) + `"}`)
	}
	if err := manager.Store().withStoreContext(t.Context(), scope.Plugin, func(root *os.Root) error {
		return storeWrite(root, "accounts.json", records, storeFileLimit)
	}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if oversize && info.Size() != storeSnapshotLimit+1 || !oversize && info.Size() >= storeSnapshotLimit {
		t.Fatalf("unexpected fixture account file size %d for oversize=%v", info.Size(), oversize)
	}
	return info.Size()
}

func countProviderStreamReads(t *testing.T, files []string, run func()) []int {
	t.Helper()
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	watches := make(map[uint32]int, len(files))
	for index, file := range files {
		watch, err := unix.InotifyAddWatch(fd, file, unix.IN_OPEN|unix.IN_CLOSE_NOWRITE)
		if err != nil {
			t.Fatal(err)
		}
		watches[uint32(watch)] = index
	}
	run()
	counts := make([]int, len(files))
	var events [4096]byte
	for {
		size, err := unix.Read(fd, events[:])
		if err == unix.EAGAIN {
			return counts
		}
		if err == unix.EINTR {
			continue
		}
		if err != nil || size == 0 || size%unix.SizeofInotifyEvent != 0 {
			t.Fatalf("read stream file events: %d bytes, %v", size, err)
		}
		for offset := 0; offset < size; offset += unix.SizeofInotifyEvent {
			watch := binary.NativeEndian.Uint32(events[offset:])
			mask := binary.NativeEndian.Uint32(events[offset+4:])
			index, ok := watches[watch]
			if !ok || mask != unix.IN_OPEN && mask != unix.IN_CLOSE_NOWRITE {
				t.Fatalf("unexpected stream file event: watch=%d mask=%#x", watch, mask)
			}
			if mask == unix.IN_CLOSE_NOWRITE {
				counts[index]++
			}
		}
	}
}

func providerStreamRequest(chunks int) sdk.ExecuteRequest {
	return sdk.ExecuteRequest{Protocol: "chat", Model: "fixture-stream-" + strconv.Itoa(chunks)}
}

func providerStreamChunk(index int) json.RawMessage {
	raw, _ := json.Marshal([]byte(strconv.Itoa(index)))
	return raw
}

func providerStreamSelection(t *testing.T, ctx context.Context, manager *Manager, plugin string) (context.Context, *workerClient) {
	t.Helper()
	lease, err := manager.Acquire(ctx, plugin)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	return WithProviderExecuteSelection(ctx, lease.Revision(), lease.ConfigVersion(), lease.worker.entry.EnableGeneration), lease.client
}

func TestProviderStreamStopsAfterPeerAccountReplacement(t *testing.T) {
	const peerRoot = "MAGPIE_TEST_PROVIDER_STREAM_ROOT"
	const peerScope = "MAGPIE_TEST_PROVIDER_STREAM_SCOPE"
	if root := os.Getenv(peerRoot); root != "" {
		var scope sdk.Scope
		if err := json.Unmarshal([]byte(os.Getenv(peerScope)), &scope); err != nil {
			t.Fatal(err)
		}
		_, err := NewStore(root).SaveAccount(Account{
			Plugin: scope.Plugin, Provider: scope.Provider, ID: scope.Account, Auth: json.RawMessage(`{"fixture":"replacement"}`),
		}, scope.Generation)
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Setenv("MAGPIE_NO_ACCOUNTS", "0")
	manager, entry := fixtureManagerAt(t, 9)
	account, err := manager.Store().SaveAccount(Account{
		Plugin: entry.Manifest.ID, Provider: "provider", ID: "selected", Auth: json.RawMessage(`{"streamFixture":true}`),
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	scope := sdk.Scope{Plugin: account.Plugin, Provider: account.Provider, Account: account.ID, Generation: account.Generation,
		Operation: "peer-replaced-stream", Attempt: 1}
	ctx, client := providerStreamSelection(t, t.Context(), manager, scope.Plugin)
	encoded, err := json.Marshal(scope)
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	peerCommitted := false
	_, err = manager.Call(ctx, scope.Plugin, sdk.MethodProviderExecute, scope, providerStreamRequest(3), func(event Event) error {
		events = append(events, event.Name)
		if event.Name != "data" || peerCommitted {
			return nil
		}
		if string(event.Data) != string(providerStreamChunk(0)) {
			return errors.New("first data chunk was changed")
		}
		command := proc.CommandContext(ctx, os.Args[0], "-test.run=^TestProviderStreamStopsAfterPeerAccountReplacement$")
		command.Env = append(os.Environ(), peerRoot+"="+manager.Namespace(), peerScope+"="+string(encoded), "GORACE=atexit_sleep_ms=0")
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("peer replacement failed: %w\n%s", err, output)
		}
		peerCommitted = true
		return nil
	})
	storeTestError(t, err, "stale_generation")
	var outcome *OutcomeError
	if !peerCommitted || !errors.As(err, &outcome) || outcome.Outcome != "uncertain" ||
		!slices.Equal(events, []string{"accepted", "headers", "data"}) {
		t.Fatalf("peer replacement published events %v, outcome %v", events, err)
	}
	journal, err := manager.Store().Operation(scope.Plugin, scope.Operation)
	if err != nil || journal.State != "uncertain" || journal.Scope != scope {
		t.Fatalf("peer replacement changed the acceptance receipt: %v, %v", journal.State, err)
	}
	scope.Generation++
	before := client.next.Load()
	_, err = manager.Call(ctx, scope.Plugin, sdk.MethodProviderExecute, scope, providerStreamRequest(3), nil)
	storeTestError(t, err, "operation_conflict")
	if client.next.Load() != before {
		t.Fatal("a previously accepted operation reached the worker again")
	}
}
