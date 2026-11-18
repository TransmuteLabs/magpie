package main

import (
	"context"
	"os"

	"github.com/yetone/magpie/internal/extension/testdata/authfixture"
	sdk "github.com/yetone/magpie/pkg/extension"
)

func main() {
	if sdk.Serve(context.Background(), os.Stdin, os.Stdout, authfixture.Module()) != nil {
		os.Exit(1)
	}
}
