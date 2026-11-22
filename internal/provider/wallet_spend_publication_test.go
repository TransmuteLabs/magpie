package provider

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/extension"
	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/filememo"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/steady"
)

func TestPluginWalletDiscoveryAtPublication(t *testing.T) {
	for _, boundary := range []string{"logins", "wallet", "flush"} {
		t.Run(boundary, func(t *testing.T) {
			login, requests := startPluginUsageFixture(t)
			if err := settings.Save(settings.Settings{}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 16*time.Second)
			defer cancel()
			seed, seeded := startWalletEditorRead(t, ctx, login.Agent, requests)
			seed <- pluginUsageReply{http.StatusOK, `{"signIn":"kept","balance":"$100","windows":[{"name":"Funds","balance":100},{"name":"Reserve","balance":50}]}`}
			checkWalletUsage(t, readWalletEditorResult(t, seeded)[login.User], 100, 50, 0, 0)
			if boundary == "wallet" {
				loginUsageCache.Lock()
				delete(loginUsageCache.m, loginReadingCacheKey(login))
				loginUsageCache.Unlock()
			} else {
				AskUsage()
			}
			path := walletSpendPath()
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			policyPath := settings.Path()
			disable := func() error {
				return os.WriteFile(policyPath, []byte(`{"noAccountDiscovery":true}`), 0o600)
			}
			reply, result := startWalletEditorRead(t, ctx, login.Agent, requests)
			releaseMemo := filememo.Hold()
			defer releaseMemo()
			if settings.AccountDiscoveryOff() {
				t.Fatal("discovery was already off")
			}
			response := pluginUsageReply{http.StatusOK, `{"signIn":"kept","user":"unverified@fake","renew":"auto","until":"2038-01-01T00:00:00Z","balance":"$80","resets":{"count":3},"windows":[{"name":"Funds","balance":80},{"name":"Reserve","balance":40}]}`}
			if boundary == "flush" {
				flushed := false
				syncFile := steady.Sync
				t.Cleanup(func() { steady.Sync = syncFile })
				steady.Sync = func(file *os.File) error {
					if err := syncFile(file); err != nil {
						return err
					}
					if stagedWriteFor(file, path) {
						flushed = true
						return disable()
					}
					return nil
				}
				reply <- response
				checkDeferredWallet(t, login, readWalletEditorResult(t, result)[login.User], errAccountDiscoveryOff.Error())
				if !flushed {
					t.Fatal("Usage did not reach the wallet flush")
				}
			} else {
				lock := &loginsMu
				if boundary == "wallet" {
					lock = &walletSpends
				}
				unlock, held := filelock.Lock(lock.Path(), time.Second)
				if !held {
					t.Fatal("could not hold the file lock")
				}
				release := sync.OnceFunc(unlock)
				defer release()
				reply <- response
				waitPluginUsagePostRead(t, "filelock.lockPath(")
				if err := disable(); err != nil {
					t.Fatal(err)
				}
				release()
				checkDeferredWallet(t, login, readWalletEditorResult(t, result)[login.User], errAccountDiscoveryOff.Error())
			}
			if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
				t.Errorf("Usage changed wallet spend after discovery was disabled: %v", err)
			}
		})
	}
}

func TestPluginWalletEditorDiscardsReplacedAccountReading(t *testing.T) {
	for _, warm := range []bool{false, true} {
		t.Run(fmt.Sprintf("warm=%t", warm), func(t *testing.T) {
			login, requests := startPluginUsageFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 16*time.Second)
			defer cancel()
			if warm {
				seed, seeded := startWalletEditorRead(t, ctx, login.Agent, requests)
				seed <- pluginUsageReply{http.StatusOK, `{"signIn":"kept","balance":"$100","windows":[{"name":"Funds","balance":100},{"name":"Reserve","balance":50}]}`}
				initial := readWalletEditorResult(t, seeded)[login.User]
				checkWalletUsage(t, initial, 100, 50, 0, 0)
				subscriptionUsageCache.Lock()
				previous := subscriptionUsageCache.data
				subscriptionUsageCache.data = []SubscriptionQuota{initial}
				subscriptionUsageCache.Unlock()
				t.Cleanup(func() {
					subscriptionUsageCache.Lock()
					subscriptionUsageCache.data = previous
					subscriptionUsageCache.Unlock()
				})
				AskUsage()
				temporary, result := startWalletEditorRead(t, ctx, login.Agent, requests)
				temporary <- pluginUsageReply{http.StatusOK, `{"signIn":"kept","error":"HTTP 503 service unavailable"}`}
				checkWalletUsage(t, readWalletEditorResult(t, result)[login.User], 100, 50, 0, 0)
				AskUsage()
			}
			reply, result := startWalletEditorRead(t, ctx, login.Agent, requests)
			saved := loginOf(t, readLogins(), login.Agent, login.User)
			saved.PluginObservations = pluginLoginObservations{Epoch: "replacement-account"}
			saveLogins(t, saved)
			reply <- pluginUsageReply{http.StatusOK, `{"signIn":"kept","user":"unverified@fake","plan":"Unverified plan","renew":"auto","until":"2038-01-01T00:00:00Z","balance":"$80","resets":{"count":3},"windows":[{"name":"Funds","balance":80}]}`}
			const reason = "the plugin account has changed"
			checkDeferredWallet(t, login, readWalletEditorResult(t, result)[login.User], reason)
			if warm {
				card := cardOf(CachedCardsContext(ctx, time.Now()), loginProvider(login), login.User)
				if card == nil {
					t.Fatal("export lost the deferred wallet card")
				}
				checkDeferredWallet(t, login, *card, reason)
			}
		})
	}
}

func TestPluginUsageEditorDiscardsReplacedNonWalletReading(t *testing.T) {
	for _, sample := range []struct{ name, windows string }{
		{"balance", `[]`},
		{"quota", `[{"name":"5 hours","used":80}]`},
	} {
		t.Run(sample.name, func(t *testing.T) {
			login, requests := startPluginUsageFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 16*time.Second)
			defer cancel()
			reply, result := startWalletEditorRead(t, ctx, login.Agent, requests)
			saved := loginOf(t, readLogins(), login.Agent, login.User)
			saved.PluginObservations = pluginLoginObservations{Epoch: "replacement-account"}
			saveLogins(t, saved)
			reply <- pluginUsageReply{http.StatusOK, `{"signIn":"kept","plan":"Prior plan","balance":"$80","renew":"auto","resets":{"count":3},"windows":` + sample.windows + `}`}
			quota := readWalletEditorResult(t, result)[login.User]
			if quota.Provider == "" || !quota.ReadDeferred || quota.Plan != "" || quota.Balance != "" || quota.Renew != "" ||
				quota.Resets != nil || len(quota.Windows) != 0 {
				t.Errorf("editor published Usage from a replaced account: %+v", quota)
			}
		})
	}
}

func TestPluginUsageEditorRejectsPreviousSignInCache(t *testing.T) {
	login, requests := startPluginUsageFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 16*time.Second)
	defer cancel()
	seed, seeded := startWalletEditorRead(t, ctx, login.Agent, requests)
	seed <- pluginUsageReply{http.StatusOK, `{"signIn":"kept","balance":"$100","renew":"auto","resets":{"count":3},"windows":[{"name":"5 hours","used":80}]}`}
	if quota := readWalletEditorResult(t, seeded)[login.User]; quota.Balance != "$100" || len(quota.Windows) != 1 {
		t.Fatalf("could not warm the editor cache: %+v", quota)
	}
	saved := loginOf(t, readLogins(), login.Agent, login.User)
	saved.PluginObservations = pluginLoginObservations{Epoch: "replacement-account"}
	saveLogins(t, saved)

	result := make(chan walletEditorResult, 1)
	go func() {
		quotas, err := LoginUsageEditor(ctx, login.Agent)
		result <- walletEditorResult{quotas: quotas, err: err}
	}()
	var read walletEditorResult
	select {
	case reply := <-requests:
		reply <- pluginUsageReply{http.StatusOK, `{"signIn":"kept","error":"HTTP 503 service unavailable"}`}
		read = <-result
	case read = <-result:
	case <-ctx.Done():
		cancel()
		<-result
		t.Fatal(ctx.Err())
	}
	if read.err != nil {
		t.Fatal(read.err)
	}
	quota := read.quotas[login.User]
	if quota.Provider == "" || quota.Balance != "" || quota.Renew != "" || quota.Resets != nil || len(quota.Windows) != 0 {
		t.Errorf("editor reused Usage from the previous sign-in: %+v", quota)
	}
	cards := CachedCardsContext(ctx, time.Now())
	if card := cardOf(cards, loginProvider(login), login.User); card != nil && (card.Balance != "" || len(card.Windows) != 0) {
		t.Errorf("export reused Usage from the previous sign-in: %+v", card)
	}
}

func TestPluginExplicitSignInReplacesUsageEpoch(t *testing.T) {
	login, _ := startPluginUsageFixture(t)
	before := loginOf(t, readLogins(), login.Agent, login.User)
	saved := plugin.Saved{Provider: "fakeco", Account: before.Home}
	PluginSignedIn(saved)
	after := loginOf(t, readLogins(), login.Agent, login.User)
	if loginID(after) != loginID(before) || after.Home != before.Home || after.PluginObservations.Epoch == "" ||
		after.PluginObservations.Epoch == before.PluginObservations.Epoch {
		t.Fatalf("explicit sign-in did not replace the Usage lifetime for the same slot: id=%s slot=%s", loginID(after), after.Home)
	}
}

func TestPluginWalletEditorDiscardsUnguardedReply(t *testing.T) {
	for _, warm := range []bool{false, true} {
		t.Run(fmt.Sprintf("warm=%t", warm), func(t *testing.T) {
			login, requests := startPluginUsageFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 16*time.Second)
			defer cancel()
			const sibling = "sibling@fake"
			key, err := plugin.Import(ctx, "fakeco", map[string]any{
				"type": "oauth", "refresh": "r-sibling", "access": "a-sibling", "expires": 9e15, "accountId": sibling,
			})
			if err != nil {
				t.Fatal(err)
			}
			saveLogins(t, append(readLogins(), savedLogin{Agent: login.Agent, User: sibling, Home: key, On: true})...)
			seed, seeded := startWalletEditorRead(t, ctx, login.Agent, requests)
			sample := pluginUsageReply{http.StatusOK, `{"signIn":"kept","balance":"$100","windows":[{"name":"Funds","balance":100},{"name":"Reserve","balance":50}]}`}
			seed <- sample
			select {
			case second := <-requests:
				second <- sample
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			initial := readWalletEditorResult(t, seeded)
			checkWalletUsage(t, initial[login.User], 100, 50, 0, 0)
			checkWalletUsage(t, initial[sibling], 100, 50, 0, 0)
			forgetLoginReadingContext(ctx, login)
			if !warm {
				loginUsageCache.Lock()
				delete(loginUsageCache.m, loginReadingCacheKey(login))
				loginUsageCache.Unlock()
			}
			before, err := os.ReadFile(walletSpendPath())
			if err != nil {
				t.Fatal(err)
			}
			reply, result := startWalletEditorRead(t, ctx, login.Agent, requests)
			lock := fmt.Sprintf("%s.use-%x.lock", plugin.AuthPath(), sha256.Sum256([]byte("fakeco")))
			if err := os.Rename(lock, lock+".held"); err != nil {
				t.Fatal(err)
			}
			defer func() {
				_ = os.Remove(lock)
				if err := os.Rename(lock+".held", lock); err != nil {
					t.Error(err)
				}
			}()
			if err := os.Mkdir(lock, 0o700); err != nil {
				t.Fatal(err)
			}
			release, denied := plugin.GuardCommunitySubscription(ctx, "fakeco")
			if denied == nil {
				release()
				t.Fatal("the owner lock was still available")
			}
			reply <- pluginUsageReply{http.StatusOK, `{"signIn":"kept","user":"unverified@fake","plan":"Unverified plan","renew":"auto","until":"2038-01-01T00:00:00Z","balance":"$80","resets":{"count":3},"windows":[{"name":"Funds","balance":80}]}`}
			got := readWalletEditorResult(t, result)
			checkDeferredWallet(t, login, got[login.User], denied.Error())
			checkWalletUsage(t, got[sibling], 100, 50, 0, 0)
			if got[sibling].User != sibling {
				t.Errorf("sibling card changed its account: %+v", got[sibling])
			}
			if after, err := os.ReadFile(walletSpendPath()); err != nil || !bytes.Equal(after, before) {
				t.Errorf("Usage without the owner lock changed the counters: %v", err)
			}
		})
	}
}

type walletEditorResult struct {
	quotas map[string]SubscriptionQuota
	err    error
}

func startWalletEditorRead(t *testing.T, ctx context.Context, agent string, requests <-chan chan pluginUsageReply) (chan<- pluginUsageReply, <-chan walletEditorResult) {
	t.Helper()
	result := make(chan walletEditorResult, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		quotas, err := LoginUsageEditor(ctx, agent)
		result <- walletEditorResult{quotas: quotas, err: err}
	}()
	t.Cleanup(func() { <-done })
	select {
	case reply := <-requests:
		return reply, result
	case read := <-result:
		t.Fatalf("account editor returned without refreshing Usage: %+v", read)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	return nil, nil
}

func readWalletEditorResult(t *testing.T, result <-chan walletEditorResult) map[string]SubscriptionQuota {
	t.Helper()
	select {
	case read := <-result:
		if read.err != nil {
			t.Fatal(read.err)
		}
		return read.quotas
	case <-time.After(8 * time.Second):
		t.Fatal("account editor did not return Usage")
		return nil
	}
}

func checkDeferredWallet(t *testing.T, login Login, quota SubscriptionQuota, reason string) {
	t.Helper()
	if quota.Provider == "" || quota.Name != "FakeCo" || quota.User != login.User || !quota.ReadDeferred || quota.Error != reason ||
		len(quota.Windows) != 0 || quota.Plan != "" || quota.Balance != "" || quota.Renew != "" || quota.Until != nil || quota.Resets != nil ||
		quota.AsOf != nil || quota.ReadAt != nil {
		t.Errorf("account editor returned a rejected wallet reading: %+v", quota)
	}
}

func TestWalletSpendAcrossProcesses(t *testing.T) {
	const mode = "MAGPIE_TEST_WALLET_PROCESS"
	const first, second = "first@fake", "second@fake"
	started := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	read := func(user string, left, spent float64, elapsed time.Duration) {
		t.Helper()
		got := withWalletSpend(t.Context(), wallet(left), user, started.Add(elapsed), walletObservation{}, t.Context().Err)
		if got.Error != "" || got.Windows[0].Spent == nil || *got.Windows[0].Spent != spent {
			t.Fatalf("%s's wallet at %v left has spent %v, error %q; want %v", user, left, shownWalletValue(got.Windows[0].Spent), got.Error, spent)
		}
	}
	if os.Getenv(mode) == "update" {
		read(first, 100, 0, 0)
		fmt.Fprintln(os.Stdout, "wallet loaded")
		if line, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil || line != "update\n" {
			t.Fatalf("parent update signal: %q, %v", line, err)
		}
		read(second, 145, 55, 2*time.Minute)
		return
	}

	signIn(t)
	read(first, 100, 0, 0)
	read(second, 200, 0, 0)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	child := proc.CommandContext(ctx, os.Args[0], "-test.run=^TestWalletSpendAcrossProcesses$", "-test.count=1")
	child.Env = append(os.Environ(), mode+"=update")
	input, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		if child.ProcessState == nil {
			_ = child.Wait()
		}
	}()
	reader := bufio.NewReader(output)
	if line, err := reader.ReadString('\n'); err != nil || line != "wallet loaded\n" {
		t.Fatalf("other process did not load the same wallet file: %q, %v", line, err)
	}
	read(first, 60, 40, time.Minute)
	if _, err := fmt.Fprintln(input, "update"); err != nil {
		t.Fatal(err)
	}
	_ = input.Close()
	log, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err != nil {
		t.Fatalf("other process could not update its wallet: %v\n%s", err, log)
	}
	raw, err := os.ReadFile(walletSpendPath())
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]walletSpend
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	for user, spent := range map[string]float64{first: 40, second: 55} {
		key := "codex/" + accountKey(user) + "/" + WindowCapID("通用积分")
		if got, ok := saved[key]; !ok || got.Spent != spent {
			t.Errorf("saved %s's wallet spent %v, present %t; want %v", user, got.Spent, ok, spent)
		}
	}
	read(second, 160, 55, 3*time.Minute)
}

func TestPluginWalletOrdersRepliesAcrossProcesses(t *testing.T) {
	const childMode = "MAGPIE_TEST_PLUGIN_WALLET_REPLY"
	for _, scenario := range []struct {
		name           string
		funds, reserve int
	}{
		{name: "new-spend", funds: 80, reserve: 40},
		{name: "same-balance", funds: 100, reserve: 50},
	} {
		if mode := os.Getenv(childMode); mode != "" {
			if mode != scenario.name {
				continue
			}
			t.Cleanup(stopPluginHost)
			t.Cleanup(plugin.Settle)
			ctx, cancel := context.WithTimeout(t.Context(), 16*time.Second)
			defer cancel()
			quota := loginQuota(ctx, Login{Agent: "plugin:fakeco", User: "usage@fake"})
			checkWalletUsage(t, quota, float64(scenario.funds), float64(scenario.reserve), float64(100-scenario.funds), float64(50-scenario.reserve))
			return
		}
		t.Run(scenario.name, func(t *testing.T) {
			login, requests := startPluginUsageFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 18*time.Second)
			defer cancel()
			response := func(funds, reserve int) pluginUsageReply {
				return pluginUsageReply{http.StatusOK, fmt.Sprintf(`{"signIn":"kept","windows":[{"name":"Funds","balance":%d,"resetSecs":3600},{"name":"Reserve","balance":%d,"resetSecs":3600}]}`, funds, reserve)}
			}
			seed, seeded := startPluginUsageRead(t, ctx, login, requests)
			seed <- response(100, 50)
			checkWalletUsage(t, readPluginUsageResult(t, seeded), 100, 50, 0, 0)
			older, olderResult := startPluginUsageRead(t, ctx, login, requests)

			child := proc.CommandContext(ctx, os.Args[0], "-test.run=^TestPluginWalletOrdersRepliesAcrossProcesses$", "-test.count=1")
			child.Env = append(os.Environ(), childMode+"="+scenario.name)
			var output bytes.Buffer
			child.Stdout, child.Stderr = &output, &output
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- child.Wait(); close(done) }()
			defer func() {
				cancel()
				<-done
			}()
			select {
			case newer := <-requests:
				newer <- response(scenario.funds, scenario.reserve)
			case err := <-done:
				t.Fatalf("newer Usage process exited before its request: %v\n%s", err, output.String())
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("newer Usage process: %v\n%s", err, output.String())
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			before, err := os.ReadFile(walletSpendPath())
			if err != nil {
				t.Fatal(err)
			}
			older <- response(90, 45)
			if quota := readPluginUsageResult(t, olderResult); !quota.ReadDeferred || quota.Error == "" || len(quota.Windows) != 0 {
				t.Errorf("superseded Usage still published its wallet: %+v", quota)
			}
			if after, err := os.ReadFile(walletSpendPath()); err != nil || !bytes.Equal(before, after) {
				t.Errorf("superseded Usage changed the wallet counters: %s → %s (%v)", before, after, err)
			}
			next, result := startPluginUsageRead(t, ctx, login, requests)
			next <- response(scenario.funds, scenario.reserve)
			checkWalletUsage(t, readPluginUsageResult(t, result), float64(scenario.funds), float64(scenario.reserve), float64(100-scenario.funds), float64(50-scenario.reserve))
		})
	}
}

func TestPluginWalletRequiresCurrentAccountLifetime(t *testing.T) {
	login, requests := startPluginUsageFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 16*time.Second)
	defer cancel()
	response := func(funds, reserve int) pluginUsageReply {
		return pluginUsageReply{http.StatusOK, fmt.Sprintf(`{"signIn":"kept","windows":[{"name":"Funds","balance":%d,"resetSecs":3600},{"name":"Reserve","balance":%d,"resetSecs":3600}]}`, funds, reserve)}
	}
	now := time.Now()
	path := walletSpendPath()
	writeFile(t, path, map[string]walletSpend{
		"fakeco/usage@fake/funds":   {Ends: now.Add(time.Hour), Since: now, Last: 100},
		"fakeco/usage@fake/reserve": {Ends: now.Add(time.Hour), Since: now, Last: 50},
	})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	older, olderResult := startPluginUsageRead(t, ctx, login, requests)
	saved := loginOf(t, readLogins(), login.Agent, login.User)
	saved.PluginObservations = pluginLoginObservations{Epoch: "replacement-account"}
	saveLogins(t, saved)
	older <- response(90, 45)
	if quota := readPluginUsageResult(t, olderResult); !quota.ReadDeferred || quota.Error == "" || len(quota.Windows) != 0 {
		t.Errorf("the old account lifetime still published a wallet: %+v", quota)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
		t.Errorf("a wallet reading from the old account lifetime changed the file: %v", err)
	}
	next, result := startPluginUsageRead(t, ctx, login, requests)
	next <- response(80, 40)
	checkWalletUsage(t, readPluginUsageResult(t, result), 80, 40, 20, 10)
}

func TestPluginWalletPublicationHoldsOwner(t *testing.T) {
	login, requests := startPluginUsageFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 16*time.Second)
	defer cancel()
	reply, result := startPluginUsageRead(t, ctx, login, requests)
	reply <- pluginUsageReply{http.StatusOK, `{"signIn":"kept","windows":[{"name":"Funds","balance":1000},{"name":"Reserve","balance":500}]}`}
	checkWalletUsage(t, readPluginUsageResult(t, result), 1000, 500, 0, 0)

	cutover := func() error {
		return plugin.WithSavedAuths(ctx, "fakeco", func(records map[string]extension.SavedSignInRecord) error {
			if len(records) != 1 {
				return fmt.Errorf("expected one source sign-in, got %d", len(records))
			}
			for key := range records {
				dir := filepath.Join(filepath.Dir(plugin.AuthPath()), "extensions", "private", ".saved-signins")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					return err
				}
				digest := strings.Repeat("a", 64)
				row := fmt.Sprintf(`[{"module":"wallet-sdk","provider":"fakeco","publicId":"wallet-sdk-provider",
					"revision":"cutover","active":true,"accounts":[{"key":%q,"id":"wallet-account","user":"usage@fake","loginId":"wallet-login"}],
					"grantRevision":%q,"digests":{%q:%q},"tickets":["wallet-ticket"]}]`, key, digest, key, digest)
				return os.WriteFile(filepath.Join(dir, "subscription-owners.json"), []byte(row), 0o600)
			}
			return fmt.Errorf("missing source sign-in")
		})
	}
	path := walletSpendPath()
	cutoverAtFlush := make(chan error, 1)
	syncFile := steady.Sync
	t.Cleanup(func() { steady.Sync = syncFile })
	steady.Sync = func(file *os.File) error {
		err := syncFile(file)
		if err == nil && stagedWriteFor(file, path) {
			cutoverAtFlush <- cutover()
		}
		return err
	}
	reply, result = startPluginUsageRead(t, ctx, login, requests)
	reply <- pluginUsageReply{http.StatusOK, `{"signIn":"kept","windows":[{"name":"Funds","balance":900},{"name":"Reserve","balance":475}]}`}
	checkWalletUsage(t, readPluginUsageResult(t, result), 900, 475, 100, 25)
	select {
	case err := <-cutoverAtFlush:
		if err == nil || !strings.Contains(err.Error(), "community subscription is busy") {
			t.Errorf("ownership transfer at the legacy wallet flush = %v, want a held ownership lock", err)
		}
	default:
		t.Fatal("legacy Usage did not reach wallet publication")
	}
	if err := cutover(); err != nil {
		t.Fatalf("ownership transfer after wallet publication: %v", err)
	}
	if err := plugin.CheckCommunitySubscription(ctx, "fakeco"); err == nil {
		t.Fatal("fixture failed to activate SDK ownership")
	}
}

func TestWalletSpendPreservesMalformedFile(t *testing.T) {
	for _, malformed := range []string{`{"unfinished":`, `null`, `{"missing-period":null}`} {
		t.Run(malformed, func(t *testing.T) {
			signIn(t)
			path := walletSpendPath()
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(malformed), 0o600); err != nil {
				t.Fatal(err)
			}
			got := withWalletSpend(t.Context(), wallet(200), "wallet@fake", time.Now(), walletObservation{}, t.Context().Err)
			if got.Error == "" || got.Windows[0].Spent != nil {
				t.Errorf("a malformed wallet file yielded a spend count or no error: %+v", got)
			}
			if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, []byte(malformed)) {
				t.Errorf("malformed wallet file was replaced: %q, %v", after, err)
			}
		})
	}
}

func TestWalletSpendCancelsFileLock(t *testing.T) {
	signIn(t)
	now := time.Now()
	const user = "wallet@fake"
	initial := withWalletSpend(t.Context(), wallet(1000), user, now, walletObservation{}, t.Context().Err)
	if initial.Error != "" || initial.Windows[0].Spent == nil {
		t.Fatalf("initial wallet reading: %+v", initial)
	}
	path := walletSpendPath()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	unlock, held := filelock.Lock(path+".lock", time.Second)
	if !held {
		t.Fatal("could not hold the wallet file lock")
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	got := withWalletSpend(ctx, wallet(800), user, now.Add(time.Second), walletObservation{}, ctx.Err)
	if got.Error != context.DeadlineExceeded.Error() || got.Windows[0].Spent != nil {
		t.Errorf("a timed-out wallet observation reported %q and spent %v", got.Error, shownWalletValue(got.Windows[0].Spent))
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
		t.Errorf("a timed-out wallet observation changed the saved balances: %q, %v", after, err)
	}
}
