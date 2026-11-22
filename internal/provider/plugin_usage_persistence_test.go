package provider

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/proc"
)

func TestPluginWalletObservationCanceled(t *testing.T) {
	for _, boundary := range []string{"mutex", "flush"} {
		t.Run(boundary, func(t *testing.T) {
			login, requests := startPluginUsageFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 16*time.Second)
			defer cancel()
			response := func(funds, reserve int) pluginUsageReply {
				return pluginUsageReply{http.StatusOK, fmt.Sprintf(`{"plan":"Saved plan","windows":[{"name":"Funds","balance":%d,"resetSecs":3600},{"name":"Reserve","balance":%d,"resetSecs":3600}]}`, funds, reserve)}
			}
			seed, seedResult := startPluginUsageRead(t, ctx, login, requests)
			seed <- response(1000, 500)
			checkWalletUsage(t, readPluginUsageResult(t, seedResult), 1000, 500, 0, 0)
			path := walletSpendPath()
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			readCtx, cancelRead := context.WithCancel(ctx)
			defer cancelRead()
			reply, result := startPluginUsageRead(t, readCtx, login, requests)
			var quota SubscriptionQuota
			switch boundary {
			case "mutex":
				walletSpends.Lock()
				release := walletSpends.Unlock
				defer func() { release() }()
				reply <- response(900, 300)
				waitPluginUsagePostRead(t, "noteWalletSpend(")
				cancelRead()
				select {
				case quota = <-result:
					release()
					release = func() {}
				case <-time.After(2 * time.Second):
					t.Error("canceled Usage remained blocked on the wallet mutex")
					release()
					release = func() {}
					quota = readPluginUsageResult(t, result)
				}
			case "flush":
				flushed := cancelHistoryFlush(t, path, cancelRead)
				reply <- response(900, 300)
				quota = readPluginUsageResult(t, result)
				if !*flushed {
					t.Fatal("Usage did not reach the wallet file flush")
				}
			}
			if quota.Error != context.Canceled.Error() {
				t.Errorf("canceled Usage reported %q", quota.Error)
			}
			if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
				t.Errorf("canceled Usage wrote wallet spend: %s (%v)", after, err)
			}
			retry, retryResult := startPluginUsageRead(t, ctx, login, requests)
			retry <- response(950, 475)
			checkWalletUsage(t, readPluginUsageResult(t, retryResult), 950, 475, 50, 25)
		})
	}
}

func checkWalletUsage(t *testing.T, quota SubscriptionQuota, funds, reserve, spentFunds, spentReserve float64) {
	t.Helper()
	if quota.Error != "" || len(quota.Windows) != 2 {
		t.Fatalf("wallet Usage: %+v", quota)
	}
	for index, want := range []struct {
		balance float64
		spent   float64
	}{{funds, spentFunds}, {reserve, spentReserve}} {
		window := quota.Windows[index]
		if window.Balance == nil || *window.Balance != want.balance || window.Spent == nil || *window.Spent != want.spent {
			t.Errorf("wallet %q: balance %v, spent %v; want balance %v, spent %v", window.Name, shownWalletValue(window.Balance), shownWalletValue(window.Spent), want.balance, want.spent)
		}
	}
}

func shownWalletValue(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

func TestPluginUsageObservationsAcrossProcesses(t *testing.T) {
	const childMode = "MAGPIE_TEST_PLUGIN_USAGE_CHILD"
	if mode := os.Getenv(childMode); mode != "" {
		t.Cleanup(stopPluginHost)
		t.Cleanup(plugin.Settle)
		ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
		defer cancel()
		switch mode {
		case "response-renewed", "response-expired", "ordinary-success":
			pp, ok := pluginOfAgentContext(ctx, "plugin:fakeco")
			if !ok {
				t.Fatal("provider not loaded before fetch")
			}
			login := loginOf(t, readLogins(), "plugin:fakeco", "usage@fake")
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, pluginBase+pp.ID+"/v1/chat/completions", strings.NewReader(`{"model":"fake-1","messages":[]}`))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := pluginFetch(pp, login.Home, req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			status := http.StatusBadGateway
			if mode == "ordinary-success" {
				status = http.StatusOK
			}
			if err != nil || resp.StatusCode != status || string(body) != "{}" || resp.Header.Get(SignInHeader) != "" {
				t.Fatalf("plugin fetch: status %d, body %q, sign-in header %q, err %v", resp.StatusCode, body, resp.Header.Get(SignInHeader), err)
			}
		case "recreated-account":
			provider, ok := pluginOfAgentContext(ctx, "plugin:fakeco")
			if !ok {
				t.Fatal("provider not loaded before sign-out")
			}
			if err := forgetPluginLogin(provider, "usage@fake"); err != nil {
				t.Fatal(err)
			}
			fallthrough
		case "explicit-sign-in", "explicit-sign-in-keeps-plan":
			account, err := plugin.Import(ctx, "fakeco", map[string]any{
				"type": "oauth", "refresh": "r-new", "access": "a-new", "expires": 9e15, "accountId": "usage@fake",
			})
			if err != nil || account == "" {
				t.Fatalf("explicit sign-in did not save an account: %q, %v", account, err)
			}
			PluginSignedIn(plugin.Saved{Provider: "fakeco", Account: account})
			provider, ok := pluginOfAgentContext(ctx, "plugin:fakeco")
			if !ok || len(pluginLoginsContext(ctx, provider)) != 1 {
				t.Fatal("signed-in account was not listed")
			}
		default:
			quota := loginQuota(ctx, Login{Agent: "plugin:fakeco", User: "usage@fake", Plan: "Saved plan"})
			if quota.Error != "" || quota.Plan != mode {
				t.Fatalf("other process's Usage = %+v, want plan %q", quota, mode)
			}
		}
		return
	}

	for _, outcome := range []struct {
		name      string
		old       pluginUsageReply
		new       pluginUsageReply
		newPlan   string
		wantPlan  string
		wantLapse bool
		wasLapsed bool
		oldFirst  bool
	}{
		{name: "changed-plan", old: pluginUsageReply{http.StatusOK, "Older plan"}, new: pluginUsageReply{http.StatusOK, "Newer plan"}, newPlan: "Newer plan", wantPlan: "Newer plan"},
		{name: "reaffirmed-plan", old: pluginUsageReply{http.StatusOK, "Older plan"}, new: pluginUsageReply{http.StatusOK, "Saved plan"}, newPlan: "Saved plan", wantPlan: "Saved plan"},
		{name: "reaffirmed-sign-in", old: pluginUsageReply{status: http.StatusUnauthorized}, new: pluginUsageReply{http.StatusOK, "Saved plan"}, newPlan: "Saved plan", wantPlan: "Saved plan"},
		{name: "reaffirmed-lapse", old: pluginUsageReply{http.StatusOK, "Saved plan"}, new: pluginUsageReply{http.StatusOK, `{"plan":"Saved plan","signIn":"expired"}`}, newPlan: "Saved plan", wantPlan: "Saved plan", wasLapsed: true, wantLapse: true},
		{name: "renewed-sign-in", old: pluginUsageReply{status: http.StatusUnauthorized}, new: pluginUsageReply{http.StatusOK, `{"plan":"Newer plan","signIn":"renewed"}`}, newPlan: "Newer plan", wantPlan: "Newer plan"},
		{name: "response-renewed", old: pluginUsageReply{status: http.StatusUnauthorized}, new: pluginUsageReply{http.StatusBadGateway, "renewed"}, wantPlan: "Saved plan"},
		{name: "response-expired", old: pluginUsageReply{http.StatusOK, "Older plan"}, new: pluginUsageReply{http.StatusBadGateway, "expired"}, wantPlan: "Older plan", wasLapsed: true, wantLapse: true},
		{name: "ordinary-success", old: pluginUsageReply{status: http.StatusUnauthorized}, new: pluginUsageReply{status: http.StatusOK}, wantPlan: "Saved plan", wantLapse: true},
		{name: "explicit-sign-in", old: pluginUsageReply{status: http.StatusUnauthorized}, wantPlan: "Saved plan"},
		{name: "explicit-sign-in-keeps-plan", old: pluginUsageReply{http.StatusOK, "Older plan"}, wantPlan: "Saved plan"},
		{name: "recreated-account", old: pluginUsageReply{status: http.StatusUnauthorized}},
		{name: "plan-only-does-not-clear-lapse", old: pluginUsageReply{status: http.StatusUnauthorized}, new: pluginUsageReply{http.StatusOK, `{"plan":"Newer plan","signIn":"kept"}`}, newPlan: "Newer plan", wantPlan: "Newer plan", wantLapse: true},
		{name: "newer-reply-arrives-last", old: pluginUsageReply{http.StatusOK, "Older plan"}, new: pluginUsageReply{http.StatusOK, "Newer plan"}, newPlan: "Newer plan", wantPlan: "Newer plan", oldFirst: true},
	} {
		t.Run(outcome.name, func(t *testing.T) {
			login, requests := startPluginUsageFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 18*time.Second)
			defer cancel()
			if outcome.wasLapsed {
				saved := loginOf(t, readLogins(), login.Agent, login.User)
				saved.Lapsed = lapsedText(mustPlugin(t), saved.User)
				saveLogins(t, saved)
			}
			oldReply, oldResult := startPluginUsageRead(t, ctx, login, requests)
			before := loginOf(t, readLogins(), login.Agent, login.User)
			mode := outcome.newPlan
			if mode == "" {
				mode = outcome.name
			}
			child := proc.CommandContext(ctx, os.Args[0], "-test.run=^TestPluginUsageObservationsAcrossProcesses$", "-test.count=1")
			child.Env = append(os.Environ(), childMode+"="+mode)
			var output bytes.Buffer
			child.Stdout, child.Stderr = &output, &output
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			childDone := make(chan struct{})
			var childErr error
			go func() {
				childErr = child.Wait()
				close(childDone)
			}()
			defer func() {
				cancel()
				<-childDone
			}()

			if outcome.new.status != 0 {
				select {
				case newReply := <-requests:
					if outcome.oldFirst {
						oldReply <- outcome.old
						readPluginUsageResult(t, oldResult)
					}
					newReply <- outcome.new
				case <-childDone:
					t.Fatalf("other process exited before reaching the vendor: %v\n%s", childErr, output.String())
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			select {
			case <-childDone:
				if childErr != nil {
					t.Fatalf("other process: %v\n%s", childErr, output.String())
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if !outcome.oldFirst {
				oldReply <- outcome.old
				readPluginUsageResult(t, oldResult)
			}
			saved := loginOf(t, readLogins(), login.Agent, login.User)
			if saved.Home != before.Home || loginID(saved) != loginID(before) {
				t.Fatalf("fixture changed the account key or stable ID: %q/%q → %q/%q", before.Home, loginID(before), saved.Home, loginID(saved))
			}
			if saved.Plan != outcome.wantPlan || (saved.Lapsed != "") != outcome.wantLapse {
				t.Errorf("saved plan %q, lapse %q; want plan %q, lapsed %v", saved.Plan, saved.Lapsed, outcome.wantPlan, outcome.wantLapse)
			}
		})
	}
}
