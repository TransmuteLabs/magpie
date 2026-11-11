package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/plugin"
)

// zcodeExecution is both the durable admission claim and the private
// binding consumed by the plugin fetch. No credential is stored here.
type zcodeExecution struct {
	Lane          string  `json:"lane"`
	TaskID        string  `json:"taskId"`
	Scope         string  `json:"scope"`
	Generation    string  `json:"generation"`
	RawModel      string  `json:"rawModel"`
	TicketID      string  `json:"ticketId,omitempty"`
	CardID        string  `json:"cardId,omitempty"`
	ExpiresAt     float64 `json:"expiresAt,omitempty"`
	ReadyDeadline float64 `json:"readyDeadline,omitempty"`
	NextDrawAt    int64   `json:"nextDrawAt,omitempty"`
	Claim         string  `json:"claim,omitempty"`
	State         string  `json:"state"`
}

type zcodeExecutionParams struct {
	TaskID     string `json:"taskId"`
	Model      string `json:"model"`
	Resolution string `json:"resolution"`
}

type publicZCodeExecution struct {
	Lane               string  `json:"lane"`
	TaskID             string  `json:"taskId"`
	RawModel           string  `json:"rawModel"`
	TicketID           string  `json:"ticketId,omitempty"`
	CardID             string  `json:"cardId,omitempty"`
	ExpiresAt          float64 `json:"expiresAt,omitempty"`
	ReadyDeadline      float64 `json:"readyDeadline,omitempty"`
	NextDrawAt         int64   `json:"nextDrawAt,omitempty"`
	State              string  `json:"state"`
	RequiresResolution bool    `json:"requiresResolution"`
}

func publicExecution(e zcodeExecution, c plugin.AccountContext) publicZCodeExecution {
	return publicZCodeExecution{
		Lane: e.Lane, TaskID: e.TaskID, RawModel: e.RawModel, TicketID: e.TicketID,
		CardID: e.CardID, ExpiresAt: e.ExpiresAt, ReadyDeadline: e.ReadyDeadline,
		NextDrawAt: e.NextDrawAt, State: e.State,
		RequiresResolution: e.Generation != c.Generation || e.Claim != "",
	}
}

func executionStatusValue(_ context.Context, e zcodeExecution, c plugin.AccountContext) any {
	return publicExecution(e, c)
}

func zcodeExecutionList(c plugin.AccountContext) (json.RawMessage, error) {
	out := struct {
		Executions []publicZCodeExecution `json:"executions"`
		NextDrawAt int64                  `json:"nextDrawAt"`
	}{Executions: []publicZCodeExecution{}}
	if err := withZCodeJournal(c, func(j *zcodeJournal) (bool, error) {
		out.NextDrawAt = j.DrawAfter
		for _, execution := range j.Executions {
			out.Executions = append(out.Executions, publicExecution(execution, c))
		}
		return false, nil
	}); err != nil {
		return nil, err
	}
	sort.Slice(out.Executions, func(i, j int) bool {
		return out.Executions[i].TaskID < out.Executions[j].TaskID
	})
	return json.Marshal(out)
}

func validZCodeExecutions(es map[string]zcodeExecution, drawAfter int64) bool {
	if drawAfter < 0 {
		return false
	}
	for task, e := range es {
		if task == "" || e.TaskID != task || e.Scope == "" || e.Generation == "" || e.RawModel == "" || e.ExpiresAt < 0 || e.ReadyDeadline < 0 || e.NextDrawAt < 0 {
			return false
		}
		if e.Lane != "offpeak" && e.Lane != "highspeed" {
			return false
		}
		switch e.State {
		case "pending", "unknown", "settling":
			if e.Claim == "" {
				return false
			}
		case "queued", "ready", "active", "expired", "settled", "not_found":
			if e.Lane == "offpeak" && e.TicketID == "" || e.Lane == "highspeed" && e.CardID == "" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func zcodeModel(model, lane string) (string, error) {
	model = strings.TrimSpace(model)
	if strings.HasSuffix(strings.ToLower(model), "-offpeak") {
		if lane != "offpeak" {
			return "", errors.New("model alias selects the wrong execution lane")
		}
		model = model[:len(model)-len("-OffPeak")]
	} else if strings.HasSuffix(strings.ToLower(model), "-highspeed") {
		if lane != "highspeed" {
			return "", errors.New("model alias selects the wrong execution lane")
		}
		model = model[:len(model)-len("-Highspeed")]
	}
	switch {
	case strings.EqualFold(model, "GLM-5.3"):
		return "GLM-5.3", nil
	case strings.EqualFold(model, "GLM-5.3-Flash"):
		return "GLM-5.3-Flash", nil
	default:
		return "", errors.New("native execution requires GLM-5.3 or GLM-5.3-Flash")
	}
}

func executionParams(params any) (zcodeExecutionParams, error) {
	var p zcodeExecutionParams
	b, err := json.Marshal(params)
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, errors.New("invalid native execution parameters")
	}
	if p.TaskID == "" || strings.TrimSpace(p.TaskID) != p.TaskID {
		return p, errors.New("a nonempty exact conversation taskId is required")
	}
	return p, nil
}

func zcodeExecutionStart(ctx context.Context, providerID, account string, c plugin.AccountContext, p zcodeExecutionParams, lane string, continued bool) (json.RawMessage, error) {
	model, err := zcodeModel(p.Model, lane)
	if err != nil {
		return nil, err
	}
	if c.Kind != "individual" && c.Kind != "team" {
		return nil, errors.New("this execution lane requires a signed-in Coding Plan account")
	}
	unlock, err := zcodeFence(c, "execution")
	if err != nil {
		return nil, err
	}
	defer unlock()
	claim, err := zcodeJournalKey()
	if err != nil {
		return nil, err
	}
	e := zcodeExecution{Lane: lane, TaskID: p.TaskID, Scope: c.Scope, Generation: c.Generation, RawModel: model, Claim: claim, State: "pending"}
	err = withZCodeJournal(c, func(j *zcodeJournal) (bool, error) {
		if old, ok := j.Executions[p.TaskID]; ok {
			if !continued || old.Lane != "offpeak" || old.State != "expired" || old.Generation != c.Generation || old.RawModel != model {
				return false, errors.New("task already has an execution or an unresolved admission; inspect or resolve it first")
			}
		} else if continued {
			return false, errors.New("only an expired Off-Peak execution can continue")
		}
		if lane == "highspeed" {
			if time.Now().UnixMilli() < j.DrawAfter {
				return false, fmt.Errorf("highspeed draw is cooling down until %s", time.UnixMilli(j.DrawAfter).Format(time.RFC3339))
			}
			for _, old := range j.Executions {
				if old.Lane == "highspeed" && (old.State == "pending" || old.State == "unknown") {
					return false, errors.New("a highspeed draw has an unresolved outcome")
				}
			}
		}
		if j.Executions == nil {
			j.Executions = map[string]zcodeExecution{}
		}
		j.Executions[p.TaskID] = e
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	operation, params := "offpeak.ticket", map[string]any{"task_id": p.TaskID}
	if lane == "highspeed" {
		operation, params = "highspeed.draw", map[string]any{"task_id": p.TaskID, "model": model}
	}
	data, callErr := zcodeAccountCall(ctx, providerID, account, c, operation, params)
	var ticket struct {
		TicketID      string  `json:"ticket_id"`
		TaskID        string  `json:"task_id"`
		State         string  `json:"state"`
		ReadyDeadline float64 `json:"ready_deadline"`
	}
	var draw struct {
		Card *struct {
			CardID    string `json:"card_id"`
			TaskID    string `json:"task_id"`
			Model     string `json:"model"`
			ExpiresAt int64  `json:"expires_at"`
		} `json:"card"`
		NextDrawAt int64 `json:"next_draw_at"`
	}
	if callErr == nil {
		if lane == "offpeak" {
			if json.Unmarshal(data, &ticket) != nil || ticket.TicketID == "" || ticket.TaskID != "" && ticket.TaskID != p.TaskID || ticket.ReadyDeadline < 0 || !zcodeTicketState(ticket.State) {
				callErr = errors.New("admission returned an invalid ticket; its outcome is unknown")
			}
		} else if json.Unmarshal(data, &draw) != nil || draw.NextDrawAt < 0 || draw.Card != nil && (draw.Card.CardID == "" || draw.Card.TaskID != p.TaskID || !strings.EqualFold(draw.Card.Model, model) || draw.Card.ExpiresAt <= 0) {
			callErr = errors.New("draw returned an invalid card; its outcome is unknown")
		}
	}
	updateErr := withZCodeJournal(c, func(j *zcodeJournal) (bool, error) {
		cur, ok := j.Executions[p.TaskID]
		if !ok || cur.Claim != claim || cur.Generation != c.Generation {
			return false, errors.New("native execution claim changed while the request was in flight")
		}
		if callErr != nil {
			var ae *plugin.AccountError
			if errors.As(callErr, &ae) && ae.NextDrawAt > j.DrawAfter {
				j.DrawAfter = ae.NextDrawAt
			}
			// An absent dispatch flag is unknown. Never use it to authorize
			// a retake; keep even a known pre-dispatch refusal conservative.
			cur.State = "unknown"
			j.Executions[p.TaskID] = cur
			return true, nil
		}
		if lane == "offpeak" {
			cur.TicketID, cur.State, cur.ReadyDeadline, cur.Claim = ticket.TicketID, ticket.State, ticket.ReadyDeadline, ""
		} else {
			j.DrawAfter = draw.NextDrawAt
			if draw.Card == nil {
				delete(j.Executions, p.TaskID)
				return true, nil
			}
			cur.CardID, cur.ExpiresAt, cur.NextDrawAt, cur.State, cur.Claim = draw.Card.CardID, float64(draw.Card.ExpiresAt), draw.NextDrawAt, "active", ""
		}
		j.Executions[p.TaskID] = cur
		return true, nil
	})
	if updateErr != nil {
		return nil, fmt.Errorf("native execution outcome requires reconciliation: %w", updateErr)
	}
	if callErr != nil {
		return nil, fmt.Errorf("admission or draw outcome is unknown; inspect or resolve it before trying again: %w", callErr)
	}
	return data, nil
}

func zcodeTicketState(state string) bool {
	switch state {
	case "queued", "ready", "active", "expired", "settled", "not_found":
		return true
	}
	return false
}

func zcodeExecutionStatus(ctx context.Context, providerID, account string, c plugin.AccountContext, p zcodeExecutionParams) (json.RawMessage, error) {
	var e zcodeExecution
	if err := withZCodeJournal(c, func(j *zcodeJournal) (bool, error) {
		var ok bool
		e, ok = j.Executions[p.TaskID]
		if !ok {
			return false, errors.New("no execution for this task")
		}
		return false, nil
	}); err != nil {
		return nil, err
	}
	if e.Generation != c.Generation {
		return json.Marshal(struct {
			Execution          any  `json:"execution"`
			RequiresResolution bool `json:"requiresResolution"`
		}{Execution: executionStatusValue(ctx, e, c), RequiresResolution: true})
	}
	if e.Lane != "offpeak" || e.TicketID == "" {
		return json.Marshal(executionStatusValue(ctx, e, c))
	}
	settlementUnknown := e.Claim != ""
	if settlementUnknown {
		unlock, err := zcodeFence(c, "execution")
		if err != nil {
			return json.Marshal(executionStatusValue(ctx, e, c))
		}
		defer unlock()
	}
	data, err := zcodeAccountCall(ctx, providerID, account, c, "offpeak.status", map[string]any{"ticket_ids": []string{e.TicketID}})
	if err != nil {
		return nil, err
	}
	var status struct {
		Tickets []struct {
			TicketID       string  `json:"ticket_id"`
			TaskID         string  `json:"task_id"`
			State          string  `json:"state"`
			ActiveDeadline float64 `json:"active_deadline"`
		} `json:"tickets"`
	}
	if json.Unmarshal(data, &status) != nil {
		return nil, errors.New("invalid Off-Peak status")
	}
	for _, ticket := range status.Tickets {
		if ticket.TicketID != e.TicketID {
			continue
		}
		if ticket.TaskID != "" && ticket.TaskID != p.TaskID || !zcodeTicketState(ticket.State) || ticket.ActiveDeadline < 0 {
			return nil, errors.New("Off-Peak status did not match the task")
		}
		if settlementUnknown {
			if ticket.State != "settled" {
				return json.Marshal(struct {
					Execution     any    `json:"execution"`
					ObservedState string `json:"observedState"`
				}{Execution: executionStatusValue(ctx, e, c), ObservedState: ticket.State})
			}
			err = withZCodeJournal(c, func(j *zcodeJournal) (bool, error) {
				cur, ok := j.Executions[p.TaskID]
				if !ok || cur.Claim != e.Claim || cur.Generation != e.Generation {
					return false, errors.New("execution changed while settlement was reconciled")
				}
				delete(j.Executions, p.TaskID)
				return true, nil
			})
			if err != nil {
				return nil, err
			}
			return json.Marshal(map[string]any{"taskId": p.TaskID, "lane": "offpeak", "state": "settled"})
		}
		err = withZCodeJournal(c, func(j *zcodeJournal) (bool, error) {
			cur, ok := j.Executions[p.TaskID]
			if !ok || cur.TicketID != e.TicketID || cur.Generation != e.Generation || cur.Claim != "" {
				return false, errors.New("execution changed while status was read")
			}
			cur.State, cur.ExpiresAt = ticket.State, ticket.ActiveDeadline
			j.Executions[p.TaskID], e = cur, cur
			return true, nil
		})
		if err != nil {
			return nil, err
		}
		break
	}
	return json.Marshal(executionStatusValue(ctx, e, c))
}

func zcodeExecutionFinish(ctx context.Context, providerID, account string, c plugin.AccountContext, p zcodeExecutionParams) (json.RawMessage, error) {
	unlock, err := zcodeFence(c, "execution")
	if err != nil {
		return nil, err
	}
	defer unlock()
	claim, err := zcodeJournalKey()
	if err != nil {
		return nil, err
	}
	var e zcodeExecution
	if err := withZCodeJournal(c, func(j *zcodeJournal) (bool, error) {
		var ok bool
		e, ok = j.Executions[p.TaskID]
		if !ok {
			return false, errors.New("no execution for this task")
		}
		if e.Generation != c.Generation || e.Claim != "" {
			return false, errors.New("execution is unresolved or belongs to an earlier login; resolve it explicitly")
		}
		if e.Lane == "highspeed" || e.State == "settled" {
			delete(j.Executions, p.TaskID)
		} else {
			e.Claim, e.State = claim, "settling"
			j.Executions[p.TaskID] = e
		}
		return true, nil
	}); err != nil {
		return nil, err
	}
	if e.Lane == "highspeed" || e.State == "settled" {
		return json.Marshal(map[string]any{"finished": true, "lane": e.Lane, "nextDrawAt": e.NextDrawAt})
	}
	data, callErr := zcodeAccountCall(ctx, providerID, account, c, "offpeak.settle", map[string]any{"ticket_id": e.TicketID})
	updateErr := withZCodeJournal(c, func(j *zcodeJournal) (bool, error) {
		cur, ok := j.Executions[p.TaskID]
		if !ok || cur.Claim != claim || cur.Generation != c.Generation {
			return false, errors.New("execution changed while settling")
		}
		if callErr == nil {
			delete(j.Executions, p.TaskID)
		} else {
			cur.State = "unknown"
			j.Executions[p.TaskID] = cur
		}
		return true, nil
	})
	if updateErr != nil {
		return nil, fmt.Errorf("settlement outcome requires reconciliation: %w", updateErr)
	}
	if callErr != nil {
		return nil, fmt.Errorf("settlement outcome is unknown; inspect or resolve it before trying again: %w", callErr)
	}
	return data, nil
}

// zcodeExecutionFor is called only for the four advertised ZCode aliases.
// It never admits, draws, or falls back to an ordinary paid selection.
func zcodeExecutionFor(ctx context.Context, providerID, accountKey, sessionID, requestedModel string) (json.RawMessage, error) {
	if sessionID == "" || strings.TrimSpace(sessionID) != sessionID {
		return nil, errors.New("native execution requires the conversation's taskId")
	}
	lane := "offpeak"
	if strings.HasSuffix(strings.ToLower(requestedModel), "-highspeed") {
		lane = "highspeed"
	} else if !strings.HasSuffix(strings.ToLower(requestedModel), "-offpeak") {
		return nil, errors.New("not a native execution model alias")
	}
	model, err := zcodeModel(requestedModel, lane)
	if err != nil {
		return nil, err
	}
	c, err := zcodeContext(ctx, providerID, accountKey)
	if err != nil {
		return nil, err
	}
	var e zcodeExecution
	if err := withZCodeJournal(c, func(j *zcodeJournal) (bool, error) {
		var ok bool
		e, ok = j.Executions[sessionID]
		if !ok || e.Lane != lane || e.RawModel != model || e.Scope != c.Scope || e.Generation != c.Generation || e.Claim != "" {
			return false, errors.New("no matching admitted native execution for this account, login, task and model")
		}
		if e.Lane == "offpeak" && e.State == "queued" {
			return false, errors.New("Off-Peak ticket is queued; check execution.status until it is ready")
		}
		now := float64(time.Now().UnixMilli())
		if e.State == "expired" || e.State == "settled" || e.State == "not_found" ||
			e.Lane == "offpeak" && (e.ExpiresAt > 0 && e.ExpiresAt <= now ||
				e.State == "ready" && e.ReadyDeadline > 0 && e.ReadyDeadline <= now) {
			return false, errors.New("native execution expired; check execution.status before continuing or start a new task")
		}
		return false, nil
	}); err != nil {
		return nil, err
	}
	// State and the journal's claim are private bookkeeping, not fetch input.
	return json.Marshal(struct {
		Lane          string  `json:"lane"`
		TaskID        string  `json:"taskId"`
		Scope         string  `json:"scope"`
		Generation    string  `json:"generation"`
		RawModel      string  `json:"rawModel"`
		TicketID      string  `json:"ticketId,omitempty"`
		CardID        string  `json:"cardId,omitempty"`
		ExpiresAt     float64 `json:"expiresAt,omitempty"`
		ReadyDeadline float64 `json:"readyDeadline,omitempty"`
		NextDrawAt    int64   `json:"nextDrawAt,omitempty"`
	}{e.Lane, e.TaskID, e.Scope, e.Generation, e.RawModel, e.TicketID, e.CardID, e.ExpiresAt, e.ReadyDeadline, e.NextDrawAt})
}
