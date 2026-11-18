package sessions

import (
	"context"
	"errors"
	"slices"
)

type SessionSourceStatus struct {
	Errors   []string               `json:"errors"`
	Pending  bool                   `json:"pending"`
	Bindings []SessionSourceBinding `json:"bindings"`
}

func InspectSessionSources(ctx context.Context) SessionSourceStatus {
	err := PrepareSessionSources(ctx)
	problems := SessionSourceErrors()
	if problems == nil {
		problems = []string{}
	}
	if err != nil && !errors.Is(err, ErrSessionSourcePending) && len(problems) == 0 {
		problems = append(problems, err.Error())
	}
	bindings, bindingErr := readSourceBindings()
	if bindingErr != nil && !slices.Contains(problems, bindingErr.Error()) {
		problems = append(problems, bindingErr.Error())
	}
	if bindings == nil {
		bindings = []SessionSourceBinding{}
	}
	return SessionSourceStatus{Errors: problems, Pending: errors.Is(err, ErrSessionSourcePending), Bindings: bindings}
}
