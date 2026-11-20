# Extension SDK decisions and historical evidence

Start with the [subsystem index](../subsystems/README.md) for current behavior,
the [executable-extension owner](../subsystems/executable-extensions.md) for
installation and lifecycle, and the [Go](../../pkg/extension/README.md) or
[JS/TS](../../sdk/js/README.md) guide for author contracts.

This index preserves decision rationale and snapshot-bound verification.
It is not a release checklist, a new acceptance result or an instruction to
reproduce intermediate implementation checkpoints. Original filenames,
including decision/validation records sharing a number, remain distinct.
The status and qualifications inside each dated record belong to its own
source and environment.

## Architectural decisions

| Record | Responsibility or decision |
| --- | --- |
| [0001: protocol extensions](0001-protocol-extensions.md) | Explicit immutable codec registries |
| [0002: SDK core](0002-upstream-sdk-core.md) | Host kernel and separately packaged customization |
| [0003: external runtime](0003-external-extension-runtime.md) | Installed manifests, scoped executable contracts and host authority |
| [0004: native ABI](0004-native-extension-abi.md) | Native Go through an SDK-owned C ABI |
| [0005: component contract](0005-composite-module-contract.md) | One root authority, private services, compatibility and lifecycle |
| [0006: surface contracts](0006-extension-surface-contracts.md) | Typed contributions through existing host owners |
| [0007: credential refresh](0007-transactional-account-refresh.md) | Durable dispatch, rotating credentials and atomic publication |
| [0008: provider operations](0008-provider-operation-contracts.md) | Finite operations distinct from conversational protocols |
| [0010: browser facade](0010-browser-extension-facade.md) | Browser SDK over the existing page bridge |
| [0011: native executor](0011-native-executor-constraints.md) | Same-module execution on the original inbound lease |
| [0012: routing strategies](0012-sdk-routing-strategies.md) | Scoped ordering at the ordinary routing point |
| [0013: usage and cancellation](0013-usage-revisions-and-business-cancel.md) | Usage freshness and separate business cancellation intent |
| [0015: selected-account windows](0015-selected-account-window-bindings.md) | Explicit selected-account grants for window actions |
| [0016: agent profiles](0016-sdk-agent-profiles.md) | Agent launch, profile ownership and Library targets |
| [0017: authentication broker](0017-window-auth-broker.md) | Host-owned polling flows and private credential publication |
| [0018: account workflow](0018-window-account-workflow.md) | Browser selection tied to host account discovery |
| [0019: session sources](0019-jsonl-session-sources.md) | Bounded local JSONL reading and explicit host binding |
| [Native account operations](native-account-operations.md) | Finite operations through an owned agent and original inbound lease |
| [0022: decisions](0022-sdk17-decision-operation.md) | Selected-account decision operation and model association |
| [0023: observability](0023-sdk-execution-observability.md) | Execution accounting and bounded structured diagnostics |
| [0024: check-in and activation](0024-sdk-account-checkin-gateway-activation.md) | Ordinary check-in and gateway component lifetime |
| [0025: own calls and history](0025-sdk-own-calls-and-outside-history.md) | Separate live accounting and outside-history ingestion |
| [0028: request recovery](0028-shared-request-recovery-policy.md) | Shared retry authority, response facts and final refusal disposition |
| [0030: linked core SDK contracts](0030-linked-core-sdk-contracts.md) | Shared owner contracts, linked implementations and rollout with empty catalogs |

These records explain why an owner or fence exists. Current minimum API
versions, descriptors, ordinary entry paths and failure behavior remain in
the owning subsystem and public SDK references; this index does not create
another contract-version authority.

## Historical validation records

| Record | Evidence to read within its original scope |
| --- | --- |
| [SDK core split](0002-upstream-sdk-core-validation.md) | Initial split environment and gate qualifications |
| [Executable SDK](0003-external-extension-runtime-validation.md) | Initial installed-worker/host proofs, original gateway failures and corrective evidence |
| [Native and composite](0005-native-composite-validation.md) | Frozen native/helper/window inputs, aggregate results and limitations |
| [Gateway hooks](0006-gateway-hooks-validation.md) | API 1.2 source checkpoints, raw aggregate failures and later review corrections |
| [SDK 1.3](0009-sdk13-validation.md) | Combined policy, lifecycle, language and platform gates |
| [SDK 1.4](0014-sdk14-validation.md) | Routing checkpoint and separately identified application follow-ups |
| [Session-source preview](0020-session-source-preview-validation.md) | Isolated reader preview and its limited integration scope |
| [Consolidated SDK 1.6](0021-consolidated-sdk16-validation.md) | Shared host snapshot, split gates, retained setup failures and follow-ups |
| [SDK 1.9 host](0026-sdk19-host-validation.md) | Synthetic combined host snapshot and unperformed deployment work |
| [SDK 1.9 integration](0027-sdk19-upstream-module-validation.md) | Named upstream/module snapshots, retained failures and later integration records |
| [Magpie and SDK acceptance](0029-sdk112-local-validation.md) | Tested source trees, host checks, empty-catalog application smoke and module acceptance |

Decision records can also contain scoped verification. Their evidence is
subject to the same source and environment boundaries as the validation
records above. Linking a historical pass does not establish a pass on a later
tree; a correction does not turn the earlier raw red into a green receipt.

For a new acceptance claim, identify the exact source/manifest, command,
declared matrix, terminal result, source-after and process/lifetime closure.
Keep failed, skipped, interrupted and unrun work visible. Cross-build success
does not prove native execution, synthetic vendors do not prove real sign-in
or billing, and an artifact check does not prove installed-app acceptance.
The integration owner maintains the current acceptance packet separately.
