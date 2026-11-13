package main

import (
	"github.com/yetone/magpie/internal/extension/testdata/authfixture"
	"github.com/yetone/magpie/pkg/extension/native"
)

func init() {
	module := authfixture.Module()
	module.Manifest.Runtime, module.Manifest.Entry = "native", "primary.so"
	if err := native.Register(module); err != nil {
		panic(err)
	}
}

func main() {}
