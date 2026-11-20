package gateway

import (
	"context"
	"errors"

	extensionhost "github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/gwmw"
	"github.com/yetone/magpie/internal/masking"
	"github.com/yetone/magpie/internal/redact"
	"github.com/yetone/magpie/internal/settings"
)

var errExtensionMasking = errors.New("the selected masking rule set is unavailable; nothing was sent")

type selectedMaskingKey struct{}

type selectedMasking struct {
	manager          *extensionhost.Manager
	selection        string
	revision         string
	enableGeneration string
}

func extensionMaskingFor(ctx context.Context, selection string, m gwmw.Masker) (context.Context, gwmw.Masker, error) {
	if selection == "" {
		return context.WithValue(ctx, selectedMaskingKey{}, selectedMasking{}), m, nil
	}
	switch m.(type) {
	case masking.Masker, *masking.Masker:
	default:
		return ctx, nil, errExtensionMasking
	}
	binding, bound := ctx.Value(protocolRegistryCtx{}).(protocolBinding)
	var manager *extensionhost.Manager
	if bound {
		if binding.server != nil {
			manager = binding.server.extensions
		}
	} else {
		manager = extensionhost.Default()
	}
	if manager == nil {
		return ctx, nil, errExtensionMasking
	}
	selected, ok := manager.MaskingRules(ctx, selection)
	if !ok {
		return ctx, nil, errExtensionMasking
	}
	rules := make([]redact.DeclarativeRule, 0, len(selected.Rules))
	for _, rule := range selected.Rules {
		p, err := rule.Compile()
		if err != nil {
			return ctx, nil, errExtensionMasking
		}
		rules = append(rules, redact.DeclarativeRule{Kind: rule.Kind, Pattern: p})
	}
	ctx = masking.WithExtensionRules(ctx, rules, redact.DeclarativeScope{
		Selection: selection, Revision: selected.Revision, EnableGeneration: selected.EnableGeneration,
	})
	ctx = context.WithValue(ctx, selectedMaskingKey{}, selectedMasking{manager, selection, selected.Revision, selected.EnableGeneration})
	return ctx, m, nil
}

func checkExtensionMasking(ctx context.Context) error {
	selected, ok := ctx.Value(selectedMaskingKey{}).(selectedMasking)
	if !ok {
		return nil
	}
	cfg, err := settings.SharedChecked()
	if err != nil || cfg.RedactRuleSet != selected.selection {
		return errExtensionMasking
	}
	if selected.selection == "" {
		return nil
	}
	current, available := selected.manager.MaskingRules(ctx, selected.selection)
	if ctx.Err() != nil || !available || current.Revision != selected.revision || current.EnableGeneration != selected.enableGeneration {
		return errExtensionMasking
	}
	return nil
}
