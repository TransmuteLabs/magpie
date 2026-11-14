# SDK execution accounting and structured diagnostics

Date: November 14, 2026.
Status: implemented slice, combined verification pending.

## Problem

The SDK executor could report cumulative usage without the ordinary media
owners consuming it. A failure after audio headers was recorded as successful,
and the conversational terminal-operation branch ended before `Watch.Attempt`.
The final ledger was marked watched, so an OTLP observer correctly avoided
counting that ledger row a second time but had no attempt to export.

Four scalar counters also cannot describe measured zero versus absent facts,
cache TTL, deferred usage, reasoning, vendor credits or an amount in a vendor's
own currency. Forwarding worker stderr would expose request bodies or secrets
without supplying trustworthy request/account identity.

## Decision

Keep the ordinary request, routing, usage, observer and budget owners. There is
one ledger row per billable execution and no SDK-only accounting database.
Each execution resets its normalized snapshot and binds it to the selected
module, provider, account and credential generation. Deep copies isolate
received events, terminal results and readers.

Legacy `Usage` retains a cumulative snapshot of its four counters. A present
final snapshot replaces event snapshots, including zero. An absent terminal
snapshot preserves the last event. API 1.7 adds an optional `facts` envelope:

- Optional token counters distinguish absent from measured zero.
- Cache writes may include an explicit one-hour subset with its total.
- Reasoning and deferred counters retain their ordinary accounting meaning.
- Served model, upstream provider, request/response IDs, stop, tier and error
  type remain vendor evidence, independent from the selected host identity.
  The vendor response ID is retained as `upstream_response_id`. The ordinary
  `response_id` remains the response object actually delivered to the client,
  including protocol translation, and stays absent when no reply was released.
- Credits preserve their plan unit. Reported cost preserves amount and currency;
  only an explicitly USD amount becomes the existing USD `OwnCost` override.
  Other currencies do not acquire a guessed exchange rate.

Facts are sparse relative to the raw response. A final facts envelope is a new
snapshot, not an increment and not a patch to an earlier event envelope.
Unspecified facts retain raw response evidence. Mixing the legacy nonzero
counters/cost with a facts envelope is refused. Counts, metadata and amounts are
bounded and validated in both author SDKs and at the actual host executor.

Media owners finish reading and closing the response before deciding its final
accounting status. A late failure updates Recent, route and ledger but appends
no JSON error to audio already released. Accepted or uncertain work retains
the existing no-replay fence. A non-SDK fallback never inherits earlier SDK
totals.

API 1.7 also adds `diagnostics.emit`, a callback available only under a live
scoped call. It accepts a bounded level, code, phase, status, duration, count and
optional retry fact. The host supplies module/component/account/operation
identity. There is no free-text, request body, credential or caller-selected
identity field. A per-module budget admits at most sixty entries per minute.
Entries use the existing application logger; raw worker stderr remains
discarded. This is operational diagnostic data, not a business-write receipt.

## Compatibility and boundaries

Packages without SDK metadata retain the legacy JS/TS owner. Existing SDK
snapshots without facts retain their meaning. Empty catalogs activate no
external module. These changes do not alter native ABI version, authentication
authority, current update feed or the macOS local-signing policy.

Incoming vendor bytes remain the provider's protocol. Normalized accounting
facts primarily describe Magpie's ledger and diagnostics; they do not authorize
arbitrary rewriting of the client's response or cross-account history import.
Reported outside calls, global observers and background scheduling have
separate owner contracts.

## Evidence

The ordinary Front media regression failed on the previous code for all five
tested operation families and late audio failure. The initial four-counter
repair, event-only/final-zero and raw-fallback controls passed 220 race20 test
events on usbox, without skips or races. Its exact scoped source and historical
failed compile attempt remain in `/tmp/magpie-startup-policy-znuo1uaa`.

The terminal Watch repair has an unchanged old-code negative through the real
OTLP exporter: the missing attempt produced billed `0/0` instead of `48/3`.
Its scoped repair passed 160 race20 events and eleven neighboring controls.
The packet is `/tmp/magpie-sdk-terminal-watch.CGp8yQH0` on usbox.

Those checkpoints precede the complete richer accounting and diagnostic
integration. Frozen combined acceptance, causal negatives, cross-platform
checks and final source identity are recorded separately before delivery.
Live vendor clients, macOS Keychain/signing and application replacement are
reserved for the subsequent live test phase.
