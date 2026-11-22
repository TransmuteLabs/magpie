package extension

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func BenchmarkStoreAccountRead(b *testing.B) {
	for _, count := range []int{1, 10, 100} {
		for _, size := range []int{0, storeSnapshotLimit + 1} {
			label := "compact"
			if size > 0 {
				label = "above-small-limit"
			}
			b.Run(fmt.Sprintf("%d-accounts/%s", count, label), func(b *testing.B) {
				store := NewStore(b.TempDir())
				scope := storeTestScope()
				data := accountSnapshotFixture(b, scope, count, size)
				path := writeAccountSnapshotFixture(b, store, scope.Plugin, data)
				root, err := os.OpenRoot(filepath.Dir(path))
				if err != nil {
					b.Fatal(err)
				}
				b.Cleanup(func() { _ = root.Close() })

				b.Run("ReadAndHash", func(b *testing.B) {
					want := sha256.Sum256(data)
					b.ReportAllocs()
					b.SetBytes(int64(len(data)))
					for b.Loop() {
						current, _, err := storeReadFile(root, "accounts.json", storeFileLimit)
						if err != nil || sha256.Sum256(current) != want {
							b.Fatalf("account file read or hash failed: %v", err)
						}
					}
				})
				b.Run("Decode", func(b *testing.B) {
					b.ReportAllocs()
					b.SetBytes(int64(len(data)))
					for b.Loop() {
						rows, err := storeDecodeAccounts(data, scope.Plugin)
						if err != nil || len(rows.Accounts) != count {
							b.Fatalf("account decode returned %d rows: %v", len(rows.Accounts), err)
						}
					}
				})
				b.Run("CheckGeneration", func(b *testing.B) {
					check := func() error {
						_, err := store.readAccountContext(b.Context(), scope.Plugin, scope.Provider, scope.Account, &scope.Generation, false)
						return err
					}
					if err := check(); err != nil {
						b.Fatal(err)
					}
					b.ReportAllocs()
					b.SetBytes(int64(len(data)))
					for b.Loop() {
						if err := check(); err != nil {
							b.Fatal(err)
						}
					}
				})
			})
		}
	}
}

func BenchmarkStoreAccountSnapshotRetention(b *testing.B) {
	const storeCount = 32
	for _, size := range []int{storeSnapshotLimit + 1, 128 << 10, 128<<10 + 1} {
		for _, count := range []int{1, 10, 100} {
			b.Run(fmt.Sprintf("%d-bytes/%d-accounts", size, count), func(b *testing.B) {
				scope := storeTestScope()
				root := b.TempDir()
				writeAccountSnapshotFixture(b, NewStore(root), scope.Plugin, accountSnapshotFixture(b, scope, count, size))
				var retained int64
				for b.Loop() {
					stores := make([]*Store, storeCount)
					for index := range stores {
						stores[index] = NewStore(root)
					}
					var before, after runtime.MemStats
					runtime.GC()
					runtime.ReadMemStats(&before)
					for _, store := range stores {
						if err := store.CheckAccountGenerationContext(b.Context(), scope); err != nil {
							b.Fatal(err)
						}
					}
					runtime.GC()
					runtime.ReadMemStats(&after)
					runtime.KeepAlive(stores)
					retained += int64(after.HeapAlloc) - int64(before.HeapAlloc)
				}
				b.ReportMetric(0, "ns/op")
				b.ReportMetric(float64(retained)/float64(b.N*storeCount), "retained-B/store")
			})
		}
	}
}

func accountSnapshotFixture(test testing.TB, scope sdk.Scope, count, size int) []byte {
	test.Helper()
	rows := storeAccounts{Accounts: make([]storeAccount, count)}
	for index := range rows.Accounts {
		rows.Accounts[index].Account = Account{
			Plugin: scope.Plugin, Provider: scope.Provider, ID: fmt.Sprintf("fixture-%03d", index),
			Generation: scope.Generation, Auth: json.RawMessage(`{}`),
		}
	}
	rows.Accounts[count-1].ID = scope.Account
	upgradeWindowAuthStore(&rows)
	if size > 0 {
		rows.Accounts[0].Auth = json.RawMessage(`{"padding":""}`)
		data, err := json.Marshal(rows)
		if err != nil {
			test.Fatal(err)
		}
		if len(data)+1 > size {
			test.Fatalf("account fixture needs %d bytes, budget %d", len(data)+1, size)
		}
		rows.Accounts[0].Auth = json.RawMessage(`{"padding":"` + strings.Repeat("x", size-len(data)-1) + `"}`)
	}
	data, err := json.Marshal(rows)
	if err != nil {
		test.Fatal(err)
	}
	data = append(data, '\n')
	if size > 0 && len(data) != size {
		test.Fatalf("account fixture has %d bytes, want %d", len(data), size)
	}
	return data
}

func writeAccountSnapshotFixture(test testing.TB, store *Store, plugin string, data []byte) string {
	test.Helper()
	if err := store.withStoreContext(test.Context(), plugin, func(root *os.Root) error {
		return storeWrite(root, "accounts.json", json.RawMessage(bytes.TrimSpace(data)), storeFileLimit)
	}); err != nil {
		test.Fatal(err)
	}
	return filepath.Join(store.root, "private", plugin, "accounts.json")
}
