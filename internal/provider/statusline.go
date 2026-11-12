package provider

// The line Claude Code's status line shows (#1997, kensenzhao): the model a
// session's latest turn went to, then each subscription and plan with the
// windows magpie last read. `magpie statusline` and the gateway's
// GET /v1/magpie/statusline (for a Claude Code on another computer than
// the gateway, steven_4422 running magpie in Docker) both print it from
// here, so the two never differ.

import (
	"fmt"
	"strings"
	"time"
)

// StatuslineText is the whole line: "→ model" when the session has one,
// then the cards of qs that only (or the lately served) keeps.
func StatuslineText(model string, qs []Quota, only []string, now time.Time) string {
	var parts []string
	if model != "" {
		parts = append(parts, "→ "+model)
	}
	for _, q := range StatuslineCards(qs, only) {
		parts = append(parts, StatuslineCard(q, now))
	}
	return strings.Join(parts, " | ")
}

// StatuslineCards are subscriptions and plans with windows or an explicit
// failure/freshness status: those named, else lately served, else all.
func StatuslineCards(qs []Quota, only []string) []Quota {
	var withWindows, served []Quota
	for _, q := range qs {
		if q.Kind == "balance" || len(q.Windows) == 0 && q.Error == "" && q.AsOf == nil && !q.ReadDeferred {
			continue
		}
		if len(only) > 0 && !StatuslineMatches(q, only) {
			continue
		}
		withWindows = append(withWindows, q)
		if q.LastServedAt != nil {
			served = append(served, q)
		}
	}
	if len(only) == 0 && len(served) > 0 {
		return served
	}
	return withWindows
}

func StatuslineMatches(q Quota, only []string) bool {
	for _, o := range only {
		for _, v := range []string{q.Provider, q.Name, q.User, q.Alias, q.Provider + "/" + q.User} {
			if v != "" && strings.EqualFold(v, o) {
				return true
			}
		}
	}
	return false
}

// StatuslineCard is one account: "claude a@x.com* 5h 62% 7d 41%".
func StatuslineCard(q Quota, now time.Time) string {
	name := q.Provider
	switch {
	case q.Alias != "":
		name += " " + q.Alias
	case q.User != "":
		name += " " + q.User
	}
	if q.Last {
		name += "*"
	}
	short := strings.NewReplacer(" hours", "h", " hour", "h", " days", "d", " day", "d", " · ", " ")
	cells := []string{name}
	if q.Error != "" {
		if q.Partial {
			cells = append(cells, "[partial: "+q.Error+"]")
		} else {
			cells = append(cells, "[error: "+q.Error+"]")
		}
	}
	if q.AsOf != nil {
		cells = append(cells, "[as of "+q.AsOf.Local().Format("2006-01-02 15:04")+"]")
	}
	if q.ReadDeferred {
		cells = append(cells, "[not refreshed]")
	}
	for _, w := range q.Windows {
		n := short.Replace(w.Name)
		switch {
		case w.Unlimited:
			cells = append(cells, n+" ∞")
		case w.Balance != nil:
			// a wallet's figure in its unit, never a share (#3950)
			cells = append(cells, n+" "+BalanceText(*w.Balance, w.Unit))
		case w.Text:
			// a row of text alone (a plugin's balance) has no share
			cells = append(cells, n+" "+w.Display)
		case q.AsOf == nil && !q.ReadDeferred && w.ResetsAt != nil && !w.ResetsAt.After(now):
			// its window has started again since it was read
			cells = append(cells, n+" 0%")
		default:
			cells = append(cells, fmt.Sprintf("%s %.0f%%", n, w.Used))
		}
	}
	if q.Partial {
		for _, o := range q.Observations {
			name := o.Name
			if name == "" {
				name = o.Kind
			}
			if o.Remaining != nil {
				cells = append(cells, name+" "+BalanceText(*o.Remaining, o.Unit))
			} else if o.Used != nil && o.Limit != nil {
				cells = append(cells, fmt.Sprintf("%s %g/%g %s", name, *o.Used, *o.Limit, o.Unit))
			}
		}
	}
	return strings.Join(cells, " ")
}
