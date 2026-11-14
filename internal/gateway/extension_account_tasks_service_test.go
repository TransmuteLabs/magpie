package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/budget"
	"github.com/yetone/magpie/internal/usage"
)

func TestExtensionAccountTaskPrivateWorkersKeepCanceledUsageAndBudget(t *testing.T) {
	for _, worker := range []string{"go", "js"} {
		t.Run(worker, func(t *testing.T) {
			server, manager, id, paid, count := taskGatewayFixture(t, "snapshots", worker)
			budget.Forget()
			t.Cleanup(budget.Forget)
			observer := &fakeObserver{}
			t.Cleanup(usage.Observe(observer))
			cancel, done := serveKeepAliveGateway(t, server)
			waitFor(t, "task gateway listener", func() bool { return server.Addr() != "" })
			secret, err := access.Update("add-key", access.Change{Name: "private-task-budget"})
			if err != nil {
				t.Fatal(err)
			}
			who, err := access.Check(secret)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := access.Update("limit-key", access.Change{Key: who.KeyID,
				Limit: &access.Limit{Period: "day", Tokens: 31}}); err != nil {
				t.Fatal(err)
			}
			who, err = access.Check(secret)
			if err != nil {
				t.Fatal(err)
			}
			ctx := access.WithIdentity(t.Context(), who)
			operation := scheduleGatewayTask(t, server, id, ctx, "schedule:overdue")
			select {
			case scope := <-paid:
				if scope.Operation != operation || scope.Generation != 1 {
					t.Fatal("private usage barrier belongs to another paid tick", scope)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("private task service did not acknowledge both measured snapshots")
			}
			for _, row := range usage.Load(time.Time{}) {
				if row.Kind == "cache_keepalive" {
					t.Fatal("private progress appended usage before termination", row)
				}
			}
			scheduleGatewayTask(t, server, id, ctx, "foreground")
			taskReceipt(t, manager, operation, "uncertain")
			var rows []usage.Record
			for _, row := range usage.Load(time.Time{}) {
				if row.Kind == "cache_keepalive" {
					rows = append(rows, row)
				}
			}
			if len(rows) != 1 || count.Load() != 1 {
				t.Fatal("private cancellation lost accounting or replayed paid work", rows, count.Load())
			}
			row := rows[0]
			if row.Input != 23 || row.Output != 3 || row.CacheRead != 19 || row.CacheWrite != 5 ||
				row.CacheWrite1h != 2 || row.UpstreamResponseID != "native-response" ||
				row.ResponseID != "" || row.CallerKeyID != who.KeyID || row.Operation != operation {
				t.Fatal("private cancellation changed its last measured facts or original caller", row)
			}
			observer.mu.Lock()
			observed := 0
			for _, row := range observer.rows {
				if row.Kind == "cache_keepalive" {
					observed++
				}
			}
			observer.mu.Unlock()
			if observed != 1 {
				t.Fatal("private cancellation missed the ordinary ledger observer", observed)
			}
			key := access.Key{ID: who.KeyID, Limit: who.Limit}
			waitFor(t, "private task budget reservation released", func() bool {
				state := budget.All(key, limitClock())
				return len(state) == 1 && state[0].InFlight == 0
			})
			if state := budget.All(key, limitClock()); len(state) != 1 || state[0].Tokens != 31 {
				t.Fatalf("private measured task did not spend the ordinary caller budget: state=%+v row=%+v limit=%+v", state, row, who.Limit)
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
				`{"model":"`+id+`/m1","messages":[{"role":"user","content":"foreground"}]}`))
			request.RemoteAddr = "127.0.0.1:12442"
			reply := httptest.NewRecorder()
			server.Front().ServeHTTP(reply, request.WithContext(ctx))
			if reply.Code != http.StatusTooManyRequests || count.Load() != 1 {
				t.Fatal("spent private task budget dispatched another vendor call", reply.Code, count.Load(), reply.Body)
			}
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
