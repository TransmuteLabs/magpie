package extension

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPublicSDKImportsOnlyStandardLibraryAndPublicContracts(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, walkError error) error {
		if walkError != nil {
			return walkError
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		source, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, declaration := range source.Imports {
			importPath, err := strconv.Unquote(declaration.Path.Value)
			if err != nil {
				return err
			}
			root, _, _ := strings.Cut(importPath, "/")
			if strings.Contains(root, ".") && importPath != "github.com/yetone/magpie/pkg/extension" &&
				importPath != "github.com/yetone/magpie/pkg/protocolext" {
				t.Errorf("%s imports a private or vendor package: %s", path, importPath)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
