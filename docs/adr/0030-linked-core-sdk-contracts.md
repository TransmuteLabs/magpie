# ADR 0030: Linked implementations use core SDK contracts

Status: accepted for `devzeus-sdk`.

## Decision

Magpie's subsystem owners use typed SDK contracts at their extension points.
The owner chooses an implementation and retains authority over admission,
account and source selection, cancellation, retries, writes, and accounting.
Frequently merged upstream entry points keep their existing responsibilities.
Agent and wire behavior can move into linked implementation packages in
`internal/extension/builtin`.

Linked Go implementations receive typed inputs directly. Installed modules
use the contracts and capabilities negotiated by the host, with a lease
over the selected package and configuration. The owner checks results and
publishes them through its existing path. A stale or failed selected module
follows the operation's declared failure policy.

The [feature registry](../subsystems/plugin-runtime.md) uses
`FeatureContribution` for the linked modules in `plugins/`. Its owner
reconciles dependencies, activation, settings, and reverse-order disposal.
The [coverage reference](../subsystems/extension-surface-coverage.md) lists
linked and installable adapters by operation. For example, linked feature
settings and installable module configuration have separate contracts.

## Migration

Migrate one ordinary entry point at a time. Capture the existing behavior,
use the shared contract from its owner, and move the implementation after
the entry point is covered. Register the implementation at the owning
subsystem's composition point. Keep filesystem, network, and UI authority
in the corresponding owner. External vendor packages retain their own
versions, provenance, and installation tests.

Use ordinary agent, gateway, session, Memory, Library, CLI, and GUI paths to
check the migration. Exercise the linked implementation with empty SDK and
legacy catalogs. Exercise installed implementations through their normal
discovery and admission paths, including disable, replacement, and removal.
Merge upstream before final validation and identify the tested source tree.

## Release boundary

The first rollout uses Magpie's linked implementations without installing a
package. OpenCode is the first separately installed package trial. macOS
local signing and the fork feed remain under the
[app update owner](../subsystems/app-updates.md). Fork publication policy
belongs to the fork's workflow.
