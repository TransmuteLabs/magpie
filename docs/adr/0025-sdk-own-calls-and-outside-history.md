# SDK own calls and outside account history

Date: November 14, 2026.
Status: implemented; scoped verification is recorded below. Shared union acceptance is separate.

## Decision

Use the ordinary accounting owners for two different kinds of evidence.
An SDK module's own accepted work reaches `usage.Append`, with its normal
live observers. Calls made outside Magpie reach `usage.IngestReported`,
which persists reported history without live observer or budget effects.
Neither path introduces a ledger, scheduler, account discovery service or
vendor-specific routing branch.

The contracts are in [`own_calls.go`](../../pkg/extension/own_calls.go) and
[`own-calls.d.ts`](../../sdk/js/own-calls.d.ts). Both require source API 1.8;
older manifests retain their existing behavior.

### Accepted own work

`usage.append` accepts `OwnCallRequest{Call: OwnCall}`. A call contains a
required bounded kind and model, optional HTTP status, elapsed milliseconds,
bounded error and the existing normalized `Usage`. Current and deferred
counters must also have representable sums. Rich facts use the same
currency, credits and vendor evidence meanings as
[execution accounting](0023-sdk-execution-observability.md).
The vendor response ID becomes `upstream_response_id`; it does not claim a
response delivered to a client.

The module negotiates `usage-reporter`. The host requires a live accepted
write originating in `module.call` or `tool.call`, bound to its declared
provider and selected account. The host supplies module, provider, account,
credential generation, session and operation identity. Callback parameters
cannot select identity, ledger source, caller key, provider key or `Watched`.
Reads, recall, source reads and ordinary provider execution cannot acquire
own-call authority through a nested component write.

The callback rechecks account generation, enabled package revision,
configuration version and cancellation. A parent intent admits one append;
a repeated callback returns `accepted: false`. The existing operation journal
refuses redispatch of a completed or ambiguous intent. Different intents are
ordinary different calls; this is not global vendor-request deduplication.

The typed host callback in
[`internal/extension/own_calls.go`](../../internal/extension/own_calls.go)
delivers to
[`internal/usage/extension_calls.go`](../../internal/usage/extension_calls.go).
It uses the existing best-effort `Append`. `accepted: true` acknowledges that
delivery, not durable storage or a financial receipt. Modules must measure
their own work and must not re-report gateway executions through this path.

### Outside account history

A provider explicitly declares `calls: true` with `usage-reporter` and
`account-auth`. Only that declaration adds the unary `provider.calls`
handler. The host selects a saved account and current generation; a request
contains only a fixed `since`/`until` window in epoch milliseconds and an
optional bounded opaque cursor. It carries no session or operation authority.
The window starts no earlier than January 1, 2000, has an end at or after its
start, and uses JavaScript-safe integer timestamps in both SDKs.

Each result supplies an explicit `calls` array and optional `nextCursor`.
Missing/null arrays mean a failed read. An empty array means a measured empty
page. Calls contain a vendor ID, time inside the window, model, optional
session/kind and normalized usage. Extra identity selectors and invalid
facts are refused by the Go/JS author SDK and host.

The ordinary provider adapter in
[`extension_calls.go`](../../internal/provider/extension_calls.go) pins the
installed revision and configuration, uses the selected account's existing
network policy, and rechecks the binding before, between and after pages.
It reads at most 32 pages, 500 calls per page and 5,000 calls per window.
A repeated cursor, failed page, cancellation or replacement rejects the
complete window. No partial page is published.

`ReadOutsideCalls` schedules this through its existing loop and watermark
file. The first SDK read backfills 30 days; later reads retain the existing
30-minute overlap. A cursor is local to one fixed window, not a durable
watermark. Only a complete valid window successfully ingested can advance
the account watermark. Failure to persist that watermark safely retries the
window using the ordinary reported-RID deduplication, including after the
in-memory dedup cache is reset.

Reported IDs are namespaced by module, provider and account, independently
of credential generation. Credential refresh does not duplicate history;
the same vendor ID in a different account remains independent. Vendor IDs
already recorded as normal calls for that provider/account are excluded.
Modules must still omit their gateway calls when the host has no matching
vendor request ID; no adapter can infer those missing correlations.
`AccountDiscoveryOff` suppresses SDK account discovery for this reader.

## Verification

The ordinary exec worker is
[`extensionusageworker`](../../internal/usage/testdata/extensionusageworker/main.go).
Tests install it through the real extension manager and saved-account store,
then exercise the normal `Append`, `ReadOutsideCalls`, `IngestReported`, log,
observer and watermark owners. Loopback barriers hold accepted writers and
history pages while account/configuration/package replacement or cancellation
revokes their authority.

Scoped commands, source/probe hashes, failed attempts and causal negatives
are preserved on usbox in `/tmp/magpie-sdk-own-calls.Ph2YCQpE`.
The first old-source run refused the new `calls` manifest field; that is a
contract negative and does not establish runtime accounting behavior.
Separate old-dispatch and old-outside-reader seam checks retain the new DTOs
so the unchanged behavioral regressions can reach their assertions.
Final scoped results are recorded in the accompanying verification packet.
Shared-host broad acceptance belongs to the parent union.

The final V5 snapshot passed 36 actual-owner test events, 7 usage neighbors,
27 host neighbors, 53 JavaScript tests, strict TypeScript, scoped build and
Linux/Windows/Darwin vet without an overlay. A consumer assembled only from
the declared SDK package files passed both runtime and type checks.
Full scoped race20 passed 720 test events on V4 with an overlay removing
only an unrelated unused parent import; it was not repeated on V5.
V5's public contract race20 passed 80 test events. The unchanged actual-owner
probes failed at their named assertions when the own-call dispatch or
outside-reader integration was individually removed. Logs preserve both
those negatives and earlier failed attempts.

Live vendor accounts, application replacement and native execution were not
run for this adapter. The shared native ownership guard and ABI are unchanged.
