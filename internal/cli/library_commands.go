package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/library"
)

func libraryCommandsCmd(args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	switch {
	case len(args) == 1 && args[0] == "available":
		commands, err := extension.Default().LibraryCommands(ctx)
		if err != nil {
			return err
		}
		return printJSON(commands)
	case len(args) == 6 && args[0] == "import" && args[4] == "--expected":
		agents, err := libraryContributionAgents(args[3])
		if err != nil {
			return err
		}
		result, err := library.ImportExtensionCommand(ctx, args[1], args[5], args[2], agents)
		if err != nil {
			return err
		}
		printLibraryResult(result)
		return nil
	default:
		return fmt.Errorf("usage:\n  %s", libraryUsage)
	}
}
