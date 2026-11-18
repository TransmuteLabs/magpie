package extension

import (
	"context"
	"io"

	"github.com/yetone/magpie/internal/proc"
)

// moduleTransport supplies bytes to the common contract owner. Process
// supervision and native-library lifetime stay in their transport adapters.
type moduleTransport struct {
	input       io.WriteCloser
	output      io.ReadCloser
	diagnostics io.ReadCloser
	stop        func()
	wait        func() error
}

func openProcessTransport(ctx context.Context, directory, program string, args []string, home string) (*moduleTransport, error) {
	command := proc.CommandContext(ctx, program, args...)
	prepareWorker(command)
	command.Dir, command.Env = directory, workerEnv(home)
	input, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	output, err := command.StdoutPipe()
	if err != nil {
		input.Close()
		return nil, err
	}
	diagnostics, err := command.StderrPipe()
	if err != nil {
		input.Close()
		output.Close()
		return nil, err
	}
	if err := command.Start(); err != nil {
		input.Close()
		output.Close()
		diagnostics.Close()
		return nil, err
	}
	return &moduleTransport{
		input: input, output: output, diagnostics: diagnostics,
		wait: func() error {
			return command.Wait()
		},
	}, nil
}
