package sessions

import (
	"context"
	"time"
)

// ResumeNow prepares the command when the user opens a listed session.
func ResumeNow(session Session) string {
	return ResumeNowContext(context.Background(), session)
}

// ResumeNowContext returns the launch command while the session's selected
// owner still has authority. A delegated command retains its source identity.
func ResumeNowContext(ctx context.Context, session Session) string {
	if session.source != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
	}
	owner, err := selectSessionHistoryOwner(ctx, session)
	if err != nil {
		return ""
	}
	admit := func() error { return owner.check(ctx) }
	run := session.Resume
	switch {
	case owner.mode == historyDelegated:
		run = sourceResume(session.source)
	case session.Agent == "codex":
		run = resumeCodex(ctx, session, admit)
	}
	if admit() != nil {
		return ""
	}
	return run
}
