# Native account operations through an owned agent binding

Status: implementation and verification in the isolated native-account-ops
worktree, pending integration into the SDK 1.6 union. Base: f4232ea76.

## Problem

An incoming native agent has account/bootstrap RPCs which are not chat,
model discovery, tools, or media. They may have no model selector in their
wire request. Translating them to dummy chat or exposing a private account
store would lose semantics or bypass the gateway's selected authority.

## Decision

SDK 1.6 adds an explicit `native-account-operations` capability and finite
`accountOperations: [{id, agent, provider, action}]` declarations. Each entry
names an existing model-bound SDK agent launch, an owned provider and a
primary `module.call` action with a declared read/write effect. Codec
contract 0.5 selects `account-operation` plus an exact declaration ID.
Declaration IDs use the same lowercase ASCII grammar as module IDs in both
Go and JS.
Existing SDK 1.5 and codec 0.4 meanings remain unchanged.

There is no arbitrary URL, path, provider or account in that request.
The core reads the current owned agent binding through the existing secure
ledger reader, then resolves its explicit visible provider/model. Group
routes are refused rather than choosing an arbitrary member. The provider
must belong to the original inbound module/manager. Its current selected
account and generation supply `Frame.Auth`, separately from parameters.

The first contract permits only the local owner without forwarded/page,
read-only, restricted, budget or concurrency-limited credential delegation.
It does not implicitly expose account data to every named gateway key.
Broader delegation requires its own policy and acceptance proof.

Decode, action and reply retain the same inbound lease. Package/config,
agent binding and account generation are checked before dispatch and again
before returning data. Reads use existing read-action restrictions; writes
use the existing durable module-call journal, acceptance and no-replay
rules. A write requires a stable native request identity. Body/reply budgets
are 1 MiB; only content type and retry-after response headers are admitted.

`AccountOperationRequest` gives the action its host-selected native model,
bounded native body and host-derived caller ID. The selected scope and Auth
remain on the SDK frame. The module owns vendor wire validation, credential
rewriting, native response semantics, domain-origin receipts and supported
method names. None of those vendor choices belongs in core.

## Consequences and rejected alternatives

- Reuse module.call instead of a second backend runtime or arbitrary fetch
  method. Existing lifecycle, account fencing and effect receipts apply.
- Use a registered generic resolver to avoid the existing agent -> gateway
  import cycle; do not duplicate private ledger parsing in the gateway.
- Preserve unknown/revoked ownership as failure, never pick another account
  or adopt manual account replacement as proven refresh.
- A scope or local key inside vendor input grants no account authority.
- Cold CLI model-catalog persistence is independent and leader-owned.
- The capability requires SDK 1.6; it is not backported into accepted 1.5.

## Verification boundary

The implementation uses isolated usbox snapshots and local synthetic peers.
Full SDK/module race suites, actual native-host operation/assignment proof,
no-replay and reconstructed application startup gates are recorded with
exact source hashes before integration. Original vendor/application parity
and live accounts are not implied by those local fixtures.
