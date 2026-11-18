package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/yetone/magpie/internal/agent"
	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/sessions"
)

const sessionResumeUsage = "usage: magpie sessions resume <source-agent> <session-id> --expected <source-identity>"

func sessionResumeCmd(args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return sessionResumeIn(ctx, args)
}

func sessionResumeIn(ctx context.Context, args []string) error {
	if len(args) != 4 || args[2] != "--expected" {
		return errors.New(sessionResumeUsage)
	}
	plan, err := sessions.PlanExtensionSourceResume(ctx, args[0], args[1], args[3])
	if err != nil {
		return err
	}
	prepared, err := agent.PrepareExtensionResume(ctx, plan.Plugin, plan.Agent)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(plan.Args, prepared.Starting.Plained) {
		return errors.New("session resume cannot disable its SDK profile")
	}
	recheck := func() error {
		if err := plan.Recheck(ctx); err != nil {
			return err
		}
		return prepared.Recheck(ctx)
	}
	if err := recheck(); err != nil {
		return err
	}
	shutdown, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	err = extension.Default().Close(shutdown)
	stop()
	if err != nil {
		return fmt.Errorf("stop SDK helpers before session resume: %w", err)
	}
	if err := recheck(); err != nil {
		return err
	}
	if plan.Cwd != "" {
		if !filepath.IsAbs(plan.Cwd) {
			return errors.New("session resume requires an absolute working directory")
		}
		previous, err := os.Getwd()
		if err != nil {
			return err
		}
		if err := os.Chdir(plan.Cwd); err != nil {
			return err
		}
		defer os.Chdir(previous)
	}
	return startRunChecked(prepared.Starting, plan.Args, recheck)
}
