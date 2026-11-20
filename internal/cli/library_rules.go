package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/library"
)

func libraryRulesCmd(args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	switch {
	case len(args) == 1 && args[0] == "available":
		rules, err := extension.Default().LibraryRules(ctx)
		if err != nil {
			return err
		}
		return printJSON(rules)
	case len(args) == 6 && args[0] == "import" && args[4] == "--expected":
		if args[3] == "" {
			return fmt.Errorf("give agent IDs or none")
		}
		result, err := library.ImportExtensionRule(ctx, args[1], args[5], args[2], agentList(args[3]))
		if err != nil {
			return err
		}
		printLibraryResult(result)
		return nil
	default:
		return fmt.Errorf("usage:\n  %s", libraryUsage)
	}
}
