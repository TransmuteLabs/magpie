# ADR 0002: Upstream kernel with external customization modules

Date: 2026-11-12

Status: accepted; branch separation completed, external runtimes remain staged.

## Decision

Maintain `devzeus` on upstream Magpie with only vendor-neutral SDK integration,
independent core fixes, and fork update/release infrastructure. The custom
OpenCode integration, intrinsic ZCode implementation, OMP session conversion
and Devin protocol implementation are not part of the kernel patch.

The old implementation is preserved on `archive/devzeus-2026-11-12` at
`893f560cccdeb8fa2e98eadb60427990c10eedcc`. That checkpoint includes the
previously uncommitted updater, signing and account-reconciliation changes.
Its separate worktree is `../magpie-devzeus-archive`.

The new branch was created at upstream
`477bbb7a15138e614bfc10f4a423608264da3e0a`. Normal scheduled upstream merges
may follow; they do not authorize reintroducing archived vendor code.
The previous SDK worktree, `../magpie-protocol-sdk`, remains available with its
Devin work. Additional index/worktree patches were saved under the repository's
Git metadata before branch changes.

## Kernel boundary

The current public SDK defines versioned capability contracts, an immutable
registry and normalized request/response interfaces. The host enforces route
ownership, authenticated callers, budgets, cancellation and catalog access.
Its integration is documented in [Protocol extensions](../subsystems/protocol-extensions.md).

No kernel switch imports or selects one of the four custom implementations.
Existing upstream agent/provider/plugin behavior remains upstream-owned.
The current Go adapters still require deliberate linkage at a composition root;
this increment does not implement external Go/Bun workers or their IPC.
Those runtimes must use the same contracts before customization modules can be
installed independently.

The future plugin boundary covers protocol codecs, provider/account
implementations, agent wiring and session migration as separate capabilities.
Generic state and operation services are a future part of the host contract;
the current host supports inbound Chat and Models only. Vendor-specific
reset/claim rules or one session format's converter belong to their modules,
rather than dedicated kernel branches.

## Retained independent changes

- Fork release workflow, custom feed behavior and optional local macOS signing.
  Release builds remain macOS-only. Publishing follows `devzeus`; manual
  versions are resolved once so the app and feed use the same version.
- Provider source snapshots and compare-before-publication writes, with
  catalog movement only after a successful rename.
- Remote quota cache namespacing by endpoint, defensive copies and freshness
  based on successful reads.
- Current MCP policy checks before reusing a recorded command.
- DSH model-alias effort handling and honest statusline/tray freshness.
- Selected-account tray-window choices and preservation of unseen saved picks.

These services contain no new vendor implementation. Their owners and
verification points are listed in [Provider integrity](../subsystems/provider-integrity.md)
and [App updates](../subsystems/app-updates.md).

## Archived or deferred

| Customization | Preserved source | Kernel disposition |
| --- | --- | --- |
| OpenCode OAuth integration | Archive, including `59acc0ca3` | Not carried into core |
| Intrinsic ZCode and native GLM compatibility | Archive, including `78235d9a9`, `ed328e2e0`, `f7cd2571b` and checkpoint fixes | Not carried into core |
| OMP bidirectional session conversion | Archive, including `4564ba7b4` | Not carried into core |
| Devin protocol/discovery work | Previous SDK worktree and saved patches | No vendor codec in core |

Account scope/generation services, partial-quota contracts and account-bound
MCP delegation need coherent SDK contracts before extraction from the archive.
Their vendor-specific consumers and UI must not be copied wholesale merely
because some helpers have generic names.

No live account, session, installed application or update-source configuration
is migrated by this branch operation. Local branch changes do not publish a
release or rewrite remote branch history.

Local checks, test isolation findings and remaining acceptance failures are
recorded in [the validation record](0002-upstream-sdk-core-validation.md).
