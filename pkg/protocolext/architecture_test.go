package protocolext

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestSDKImportsNoCorePrivateOrVendorPackages(t *testing.T) {
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
			module, _, _ := strings.Cut(importPath, "/")
			if strings.Contains(module, ".") && importPath != "github.com/yetone/magpie/pkg/protocolext" &&
				!strings.HasPrefix(importPath, "github.com/yetone/magpie/pkg/protocolext/") {
				t.Errorf("%s crosses the public SDK boundary by importing %s", path, importPath)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
