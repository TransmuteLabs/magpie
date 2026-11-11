package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/provider"
)

func zcodeResetArgs(args []string) bool {
	for _, a := range args {
		if a == "--provider" || a == "--account" || a == "--window" {
			return true
		}
	}
	return false
}

func confirmZCode(yes bool, message string) error {
	if yes {
		return nil
	}
	fmt.Print(message + " [y/N] ")
	line, _ := stdin().ReadString('\n')
	if answer := strings.ToLower(strings.TrimSpace(line)); answer != "y" && answer != "yes" {
		return errors.New("nothing changed")
	}
	return nil
}

func captureZCodeCLI(ctx context.Context, id, account string) (context.Context, error) {
	read, cancel := context.WithTimeout(ctx, provider.RemoteResetWait)
	defer cancel()
	var result plugin.AccountResult
	var err error
	if provider.IsRemoteCard(id) {
		result, err = provider.RemoteZCodeAccountOperation(read, id, account, "context", nil)
	} else {
		result, err = provider.ZCodeAccountOperation(read, id, account, "context", nil)
	}
	if err != nil {
		return ctx, err
	}
	if result.Context.Scope == "" || result.Context.Generation == "" {
		return ctx, errors.New("ZCode account scope or generation is unavailable")
	}
	return provider.WithZCodeAccountExpectation(ctx, result.Context.Scope, result.Context.Generation), nil
}

func quotaZCodeResetCmd(args []string) error {
	var id, account, window string
	yes := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--yes", "-y":
			yes = true
		case "--provider", "--account", "--window":
			flag := args[i]
			if i+1 == len(args) || strings.HasPrefix(args[i+1], "-") {
				return fmt.Errorf("%s requires a value", flag)
			}
			i++
			switch flag {
			case "--provider":
				id = args[i]
			case "--account":
				account = args[i]
			case "--window":
				window = args[i]
			}
		case "--help", "-h", "help":
			fmt.Println(quotaUsage)
			return nil
		default:
			return fmt.Errorf("unknown ZCode reset argument %q", args[i])
		}
	}
	if id == "" || account == "" || window != "five-hour" && window != "week" {
		return errors.New("name --provider, --account and --window five-hour or week")
	}
	base, err := captureZCodeCLI(provider.Asked(context.Background()), id, account)
	if err != nil {
		return err
	}
	if err := confirmZCode(yes, fmt.Sprintf("Spend one %s reset on %s / %s? This cannot be undone.", window, id, account)); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(base, provider.RemoteResetWait)
	defer cancel()
	var out provider.ZCodeResetState
	if provider.IsRemoteCard(id) {
		out, err = provider.UseRemoteZCodeReset(ctx, id, account, window, false)
	} else {
		out, err = provider.UseZCodeReset(ctx, id, account, window)
	}
	if err != nil {
		if out.Pending != "" {
			_ = printZCodeJSON(out)
		}
		return err
	}
	return printZCodeJSON(out)
}

func zcodeAccountCmd(args []string) error {
	if len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		fmt.Println(quotaUsage)
		return nil
	}
	if len(args) < 3 {
		return errors.New("usage: magpie quota account <provider> <account> <operation> [--params <JSON>] [--yes]")
	}
	id, account, operation := args[0], args[1], args[2]
	if strings.HasPrefix(id, "-") || strings.HasPrefix(account, "-") || strings.HasPrefix(operation, "-") {
		return errors.New("name a provider, an account and an operation")
	}
	var params any = map[string]any{}
	yes := false
	for i := 3; i < len(args); i++ {
		switch args[i] {
		case "--yes", "-y":
			yes = true
		case "--params":
			if i+1 == len(args) {
				return errors.New("--params requires a JSON object")
			}
			i++
			var m map[string]any
			if err := json.Unmarshal([]byte(args[i]), &m); err != nil || m == nil {
				return errors.New("--params requires a JSON object")
			}
			params = m
		default:
			return fmt.Errorf("unknown account argument %q", args[i])
		}
	}
	if strings.HasPrefix(operation, "reset.") || operation == "mcp.headers" || operation == "offpeak.ticket" || operation == "offpeak.settle" || operation == "highspeed.draw" {
		return errors.New("that internal operation is not available; use the coordinated reset or execution controls")
	}
	base, err := captureZCodeCLI(provider.Asked(context.Background()), id, account)
	if err != nil {
		return err
	}
	switch operation {
	case "usage.balance", "execution.list":
		// Read-only account projections do not require spending confirmation.
	case "selection.select", "billing.claim", "marketing.action", "execution.take", "execution.draw", "execution.continue", "execution.finish", "execution.cancel":
		if err := confirmZCode(yes, "Perform "+operation+" for "+id+" / "+account+"?"); err != nil {
			return err
		}
	case "reset-resolve", "execution.resolve":
		if err := confirmZCode(yes, "Forget an unconfirmed outcome? The vendor may already have spent or admitted it; a later attempt may spend again."); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(base, provider.RemoteResetWait)
	defer cancel()
	if operation == "reset-status" {
		if provider.IsRemoteCard(id) {
			out, err := provider.RemoteZCodeResetStatus(ctx, id, account)
			if err != nil {
				return err
			}
			return printZCodeJSON(out)
		}
		out, err := provider.ZCodeResetStatus(ctx, id, account, false)
		if err != nil {
			if out.Source != "" {
				_ = printZCodeJSON(out)
			}
			return err
		}
		return printZCodeJSON(out)
	}
	if operation == "reset-resolve" {
		if provider.IsRemoteCard(id) {
			out, err := provider.UseRemoteZCodeReset(ctx, id, account, "", true)
			if err != nil {
				return err
			}
			return printZCodeJSON(out)
		}
		if err := provider.ResolveZCodeReset(ctx, id, account); err != nil {
			return err
		}
		return printZCodeJSON(map[string]bool{"resolved": true})
	}
	if provider.IsRemoteCard(id) {
		out, err := provider.RemoteZCodeAccountOperation(ctx, id, account, operation, params)
		if err != nil {
			return err
		}
		return printZCodeJSON(out)
	}
	out, err := provider.ZCodeAccountOperation(ctx, id, account, operation, params)
	if err != nil {
		return err
	}
	return printZCodeJSON(out)
}

func printZCodeJSON(value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func zcodeObservationCell(o provider.UsageObservation) string {
	name := o.Name
	if name == "" {
		name = o.Kind
	}
	if o.Remaining != nil {
		return fmt.Sprintf("%s: %g %s left", name, *o.Remaining, o.Unit)
	}
	if o.Used != nil && o.Limit != nil {
		return fmt.Sprintf("%s: %g / %g %s", name, *o.Used, *o.Limit, o.Unit)
	}
	return name + ": unavailable"
}
