package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/yetone/magpie/internal/sessions"
)

const sessionSourcesUsage = `usage: magpie sessions sources [--json]
       magpie sessions sources bind <module> <source> --expected <package-revision>
       magpie sessions sources unbind <agent> --expected <binding-identity>`

func sessionSourcesTo(out io.Writer, args []string) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		_, err := fmt.Fprintln(out, sessionSourcesUsage)
		return err
	}
	read := len(args) == 0 || len(args) == 1 && args[0] == "--json"
	bind := len(args) == 5 && args[0] == "bind" && args[3] == "--expected"
	unbind := len(args) == 4 && args[0] == "unbind" && args[2] == "--expected"
	if !read && !bind && !unbind {
		return errors.New(sessionSourcesUsage)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if bind {
		return sessions.BindSessionSourceNative(ctx, args[1], args[2], args[4])
	}
	if unbind {
		return sessions.UnbindSessionSourceNative(ctx, args[1], args[3])
	}
	status := sessions.InspectSessionSources(ctx)
	if err := json.NewEncoder(out).Encode(status); err != nil {
		return err
	}
	if len(status.Errors) > 0 {
		return errors.New("session source inspection reported errors")
	}
	return nil
}
