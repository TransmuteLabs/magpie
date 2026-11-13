package extension

import (
	"context"
	"errors"

	sdk "github.com/yetone/magpie/pkg/extension"
	codec "github.com/yetone/magpie/pkg/protocolext"
)

type executorKey struct{}

func WithExecutorPolicy(ctx context.Context, policy codec.ExecutorPolicy) (context.Context, error) {
	if policy == "" {
		if original, required := ctx.Value(executorKey{}).(*Lease); required {
			if original == nil {
				return nil, errors.New("native executor constraint has no owned lease")
			}
			if err := CheckExecutor(ctx, original.Manifest().ID, original.manager.Namespace()); err != nil {
				return nil, err
			}
		}
		return ctx, nil
	}
	lease := inboundLeaseFrom(ctx)
	if policy != codec.SameModuleExecutor || lease == nil || lease.closed.Load() || lease.ctx.Err() != nil {
		return nil, errors.New("native executor requires a live owned inbound lease")
	}
	if original, required := ctx.Value(executorKey{}).(*Lease); required && original != lease {
		return nil, errors.New("native executor constraint cannot change its inbound lease")
	}
	manifest := lease.Manifest()
	if manifest.Contract.Version.Minor < 4 || !manifest.Contract.Has(sdk.InboundCodec) || !manifest.Contract.Has(sdk.OutboundExecutor) {
		return nil, errors.New("native module did not negotiate its executor constraint")
	}
	return context.WithValue(ctx, executorKey{}, lease), nil
}

func ExecutorRequired(ctx context.Context) bool {
	_, required := ctx.Value(executorKey{}).(*Lease)
	return required
}

func CheckExecutor(ctx context.Context, module, namespace string) error {
	lease, required := ctx.Value(executorKey{}).(*Lease)
	if !required {
		return nil
	}
	if lease == nil || lease != inboundLeaseFrom(ctx) || lease.closed.Load() || lease.ctx.Err() != nil ||
		module != lease.Manifest().ID || namespace != lease.manager.Namespace() {
		return errors.New("native request requires its original module executor")
	}
	return nil
}
