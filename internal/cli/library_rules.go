package cli

import (
	"context"
	"fmt"
	"strings"
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
		agents, err := libraryContributionAgents(args[3])
		if err != nil {
			return err
		}
		result, err := library.ImportExtensionRule(ctx, args[1], args[5], args[2], agents)
		if err != nil {
			return err
		}
		printLibraryResult(result)
		return nil
	default:
		return fmt.Errorf("usage:\n  %s", libraryUsage)
	}
}

func libraryContributionAgents(value string) ([]string, error) {
	agents := agentList(value)
	if len(agents) == 0 && strings.TrimSpace(value) != "none" {
		return nil, fmt.Errorf("give agent IDs or none")
	}
	return agents, nil
}
