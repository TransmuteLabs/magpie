package provider

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"

	extensionhost "github.com/yetone/magpie/internal/extension"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func BenchmarkHoldAccountFiles(bench *testing.B) {
	for _, count := range []int{0, 16} {
		bench.Run(strconv.Itoa(count)+"-namespaces", func(bench *testing.B) {
			bench.Setenv("XDG_CONFIG_HOME", bench.TempDir())
			store := extensionhost.NewStore(filepath.Dir(extensionhost.CatalogPath()))
			for index := range count {
				id := fmt.Sprintf("fixture-%02d", index)
				_, err := store.SaveAccount(extensionhost.Account{Plugin: id, Provider: "vendor",
					ID: "account", Auth: json.RawMessage(`{"access":"fixture"}`)}, 0)
				if err != nil {
					bench.Fatal(err)
				}
				if _, err := store.StateCAS(sdk.Scope{Plugin: id}, "module.config", 0,
					json.RawMessage(`{"region":"fixture"}`)); err != nil {
					bench.Fatal(err)
				}
			}
			Hold()()
			bench.Run("serial", func(bench *testing.B) {
				bench.ReportAllocs()
				for bench.Loop() {
					Hold()()
				}
			})
			bench.Run("concurrent", func(bench *testing.B) {
				bench.ReportAllocs()
				bench.SetParallelism(4)
				bench.RunParallel(func(parallel *testing.PB) {
					for parallel.Next() {
						Hold()()
					}
				})
			})
		})
	}
}
