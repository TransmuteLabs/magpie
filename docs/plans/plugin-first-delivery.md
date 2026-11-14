# Deliver Magpie with the SDK before installing our plugins

Date: 2026-11-14

The first rollout is Magpie with our SDK and no external plugins installed.
It must retain ordinary core operation, the fork release workflow/update feed
and local macOS update signing. Module installation, account migration and
vendor acceptance are later steps. Existing module owners continue in
parallel; their unfinished features do not block this first host candidate.
The coverage census remains the SDK backlog, not a sequential prerequisite
for every module.

## Current bases and workspaces

- Host implementations are consolidated on `devzeus`, SDK API 1.9/native
  ABI 1. The current implementation checkpoint is
  `9b55245a4ce77819984b04ab43e893de914b2632`, including upstream
  `221c91fae3ef70328ad115c445034de1cea40aa1`. Its exact tree
  `0bd9c4b3b33a792e0046b04ddc99b7656062cef7` passed safe broad
  (31,320 PASS / 109 packages), empty-catalog binary, route/startup,
  signing and platform gates on usbox.
  The [upstream and module checkpoint](../adr/0027-sdk19-upstream-module-validation.md)
  records this acceptance, preserved earlier failures and the separately
  pinned module/browser evidence. Earlier implementation `d5b098d8` and its
  preceding functional candidate retain their own receipts. The earlier
  [SDK 1.9 host checkpoint](../adr/0026-sdk19-host-validation.md)
  retains the exact acceptance of `46756a48c`, including its empty-catalog binary.
  Historical [SDK 1.6 validation](../adr/0021-consolidated-sdk16-validation.md)
  remains a record of its own source, rather than acceptance of this checkpoint.
  No further host branches or worktrees are needed for module-specific SDK
  variants. New generic fixes belong on this shared base.
- [Native/composite](../adr/0005-native-composite-validation.md),
  [gateway hooks](../adr/0006-gateway-hooks-validation.md),
  [API 1.3](../adr/0009-sdk13-validation.md) and
  [API 1.4](../adr/0014-sdk14-validation.md) records retain their exact
  historical evidence. They do not automatically validate a later tree.
- Active modules have separate adjacent repositories and a development
  replacement for the public SDK:
  `../magpie-sdk-plugin-protocol-devin`,
  `../magpie-sdk-plugin-provider-opencode`,
  `../magpie-sdk-plugin-session-omp`, and
  `../magpie-sdk-plugin-protocol-zcode`, plus
  `../magpie-sdk-plugin-protocol-commandcode`. Each owns its implementation,
  provenance, packaging and acceptance. The former `magpie-extensions`
  workspace is not used by implementation or verification.
- The archived `devzeus` history and recovery backups remain historical
  donors. Recovered uncommitted host work was inventoried and compared with
  the consolidated implementation. Devin protocol evidence is under
  `../../Agents/devinapp`.
- SDK source remains in the public fork `TransmuteLabs/magpie`, with canonical
  Go module path `github.com/yetone/magpie`. A separate SDK repository is not
  required. The adjacent module repositories use a development
  replacement `replace github.com/yetone/magpie => ../magpie`; that local
  replacement is not a downloadable release version.

The local replacements do not make unpublished commits downloadable. A
published plugin source later pins an available fork revision, while users
install compiled module distributions separately.

## First host rollout

The SDK is dormant with empty SDK and legacy catalogs. Acceptance exercises
the ordinary built CLI/gateway and the public server front with Bun
unavailable, without installing a fixture module or migrating an account.
The permanent gateway regression also checks ordinary model discovery and a
real request to a synthetic configured provider.

The [update owner](../subsystems/app-updates.md) retains the existing fork
feed and stages a verified download before optional local-key signing and
replacement. Tests of that policy do not establish real Keychain access,
macOS permission continuity or an installed-app transition. Publishing,
installation and restart remain separate from source verification.

## Independent module slices

| Lane | Current default package | Minimum contract | Acceptance boundary | Independent of |
| --- | --- | --- | --- | --- |
| Devin | 0.8.0: native paired protocol/provider, auth, agent/window, bootstrap and explicitly enabled paid cache work | API 1.9, ABI 1 | Donor protocol and installed-host fixtures have separate evidence. Final no-op/transition and paid-task replays use the combined host; live vendor acceptance remains separate | Unrelated media, built-in Memory access and other module releases |
| OpenCode | 0.6.0: native Go/Zen provider, auth, agent/window and SQLite/WAL history in one package | API 1.8, ABI 1 | Ordinary installed-host discovery, execution, Library and history must pass on the selected host. Actual client/vendor versions retain separate evidence | Unrelated SDK owners and other module releases |
| OMP sessions | 0.1.0: attachment-aware six-route conversion and JSONL Source with descendant identities in the default package | API 1.9, ABI 1 | Repaired OMP→Codex export has actual Codex 0.160 continuation evidence. A new host replay does not repeat or broaden that historical client proof | Provider execution, routing and other module releases |
| ZCode | 0.13.1: native provider/controller, auth, SQLite state, agent/window, billing and claim policy | API 1.9, ABI 1 | Recovered-client and installed-host fixtures are distinct from actual vendor startup. Source provenance records desktop 3.18.2/CLI 0.17.1 and subsequent community donor changes | Unrelated SDK owners and other module releases |
| CommandCode | 0.1.2: native provider, auth/quota, agent and dedicated System One decisions | API 1.9, ABI 1 | Jev uses the declared decision operation and remains outside conversational catalogs. Installed-host protocol, accounting and agent-option tests are separate from live vendor evidence | First plugin-free host rollout and unrelated module features |

Repository names follow `magpie-sdk-plugin-<type>-<name>`. The type names the
primary purpose, not an exclusive runtime or capability set. Each module
owns its domain code, tests, command entry, package manifest,
optional web assets and README. No module imports host `internal` packages.
There is no shared vendor utility layer until real duplication justifies one.

## Contract freeze and blocker rule

A module declares only the capabilities and minimum version it uses, rather
than copying the host's FullContract superset. The transport/SDK may grow
without adding new requirements to an existing module. A new operation,
metadata field or lifecycle guarantee requires its declared minimum floor;
native ABI remains 1. Further owner adapters are not dependencies of modules
that already have their required contracts.

When blocked, the module lane reports:

1. The exact user-visible slice it cannot complete.
2. The required input/result/state semantics and owning host subsystem.
3. The source evidence or sanitized fixture that demonstrates the gap.
4. The smallest generic contract and an acceptance test.
5. Work that remains independent and continues in the meantime.

The SDK lane implements that bounded contract; it does not bring the vendor
implementation into core. Module lanes keep working against their frozen
minimum, fixtures or explicit contract-level doubles until the host adapter
is ready. A double is recorded as such and is not actual-host proof.
Each package records its exact tested host in its provenance and acceptance
packet; a declared minimum is not evidence that the newest host was tested.

Do not respond to an unsupported vendor field by discarding it, approximating
its meaning or widening an HTTP allowlist without a defined operation
contract. Source inputs, rotating refresh, native account operations,
System One, provider outside calls and paid account tasks have bounded
generic adapters. API 1.9 also passes the upstream decision catalog to
agent adapters as `decisionModels`, separately from conversation `models`;
API 1.8 adapters keep their earlier options. An additional gap needs its own
evidence; it is not a reason to stop independent module progress.

## Parallel ownership and verification

- Five module lanes own disjoint repositories: Devin, OpenCode, OMP, ZCode and
  CommandCode.
- The SDK owner works only on agreed generic blockers.
- A verification lane runs the frozen SDK aggregate and module host gates.
- An independent reviewer checks contract boundaries and fixes.
- ZCode implementation is active after the merged upstream checkpoint; vendor
  logic stays in its external repository.

All builds and tests use `usbox`; `usbox-adm` is administration only.
Heavy aggregate verification has at most three jobs, while module lanes use
bounded focused tests and pinned shared caches. Tests use temporary
configuration, synthetic HTTP and reviewed fixtures; no live sign-ins,
accounts, sessions, installed-app replacement or vendor calls.

Report readiness per slice:

- **Domain-tested:** extracted implementation passes relevant fixtures.
- **Installable:** bundle builds and installs under the declared minimum.
- **Host-integrated:** ordinary host discovery/dispatch/off/update behavior
  passes through the actual adapter.
- **Vendor-validated:** separately authorized real-client/vendor evidence
  establishes the stated behavior.

A module can reach its first host-integrated slice while optional features
remain in development. Only that slice is advertised as ready. Local commits
are coherent per module/SDK boundary; publication and deployment are separate
actions.
