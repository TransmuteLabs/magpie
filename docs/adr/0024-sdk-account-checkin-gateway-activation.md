# Selected-account check-in and gateway activation

Status: scoped owner verification green; independent review and combined
post-merge host acceptance remain separate.

This is the next owner-adapter slice after ADR0022's System One work.
The shared SDK version owner currently coordinates unpublished API 1.8
with storage and search. This proposal uses that floor provisionally;
it neither changes `FullContract` nor claims that 1.8 is accepted.
Own-model metadata, agent controller, startup migration policy, deployment
and module activation on a user's installation remain outside this scope.

## Existing owners and gap

`internal/provider/checkin.go` owns the daily/manual check-in gate,
account exclusion, retry timing, per-provider network policy and saved
results. `plugin_checkin.go` projects legacy plugin declarations into that
owner and the existing Settings and quota cards. SDK providers do not yet
join it. Naming a tool or an arbitrary action `checkin` grants no such
registration.

`Module.Start` and its disposer already belong to a component generation.
`Manager.AcquireRevision` pins that generation and `Manager.Reload`
withdraws changed generations before their bounded drain. There is no
declared gateway activation owner that acquires and releases such a lease
while the ordinary gateway serves. `Server.New` and `Server.Front` must
keep their verified empty-catalog behavior without starting a worker,
requiring Bun, or acquiring a native module.

## Proposed minimum wire

1. A provider may declare
   `checkin: {"action": "daily.checkin", "recheck": false}`.
   The action must resolve within that module, have effect `write`, and
   use the existing `module.call` contract. Admission requires the
   negotiated minor, `account-auth` and `module-rpc`; no new capability
   or vendor identifier is added. Missing or null means no registration.
   Invalid declarations fail explicitly instead of using a legacy plugin.
2. The typed request contains the host's `day` (`YYYY-MM-DD`, using the
   ordinary check-in owner's day) and selected `proxy` choice. Credentials
   remain in `Frame.Auth`. The module owns its vendor's day and returns
   its next boundary as `until`; the host does not infer vendor rules.
3. A unary result contains finite `outcome`, optional `credit`, `streak`,
   bounded `message` and RFC3339 `until`. Outcomes are the ordinary owner's
   claimed, done, inactive, ineligible, captcha, own-app and failed states.
   Invalid numbers, text, timestamps or an oversized/non-unary result are
   rejected. Go, JavaScript, TypeScript and the actual leased invocation
   must agree.
4. An optional root `activation: ["gateway"]` is a finite list of known
   activation points. The sole initial point is `gateway`; duplicate or
   unknown points and an older negotiated minor are invalid. A private
   service manifest does not inherit it. This retains the common
   declaration/lifecycle direction from ADR0006 without unrestricted RPC.

## Check-in execution and receipts

The SDK adapter supplies ordinary `checkiner` accounts with stable keys
derived from module, provider and account ID, never a display name.
Provider/account Off, per-account exclusion, manual requests, clock gate,
provisional answers, recheck and result display remain with that owner.
The existing `Settings.PluginCheckins` map can hold the stable namespaced
SDK provider ID and defaults to false. Existing vendor switches and legacy
check-in behavior remain unchanged.

Every call rechecks the installed package/config revision and exact account
generation, then uses the selected account's refresh/network/hold policy.
A paused provider may still be checked manually under the ordinary policy;
a disabled module, removed account or revoked generation cannot dispatch.
The action receives only its declared selected-account authority. Existing
accepted-write usage invalidation remains scoped to that account.

The adapter must reserve a durable logical check-in intent before dispatch
and use the existing operation journal, including its accepted ACK. A
failed, uncertain, malformed or lost response after dispatch cannot become
a new automatic intent on the next timer, manual press, process restart or
generation change. A known completed non-claim result may permit a later
ordinary recheck; an unknown result may not. Distinct legitimate rechecks
need distinct durable intents, rather than one operation ID reused for
every call of the day. The daily display file alone is insufficient:
failure to save that file after a completed claim must not repeat the claim.

Keep the operation reference and terminal state alongside the ordinary
saved/displayed result. Do not manufacture inference usage or append a
second billing row for credit grants. Existing logs, quota cards and
operation receipts must remain available. Explicit operation reconciliation
is separate from automatic retry; this slice must expose a refusal when a
terminal intent cannot be reconciled.

## Gateway activation lifetime

After successful listener acquisition, the gateway owner starts one
revocable activation handle for its manager. It enumerates metadata first
and acquires exact revision/config leases only for enabled declarations.
An empty SDK catalog or a module without the declaration starts nothing.
Installation validation and an ordinary explicit module call retain their
existing component lifetimes; the declaration adds gateway ownership,
not another module runtime.

The handle reconciles catalog/config changes, including another process's
writes. It withdraws old activation before releasing the old lease and
starting the new generation. Disable, removal, invalid/unreadable authority,
manager shutdown and gateway shutdown revoke it. It joins its reconciliation
work and listener subscription, releases all leases and uses the existing
bounded generation drain/disposer. Releasing a lease alone is insufficient
for an otherwise cached worker: the implementation must retire the
activation's withdrawn generation without redirecting in-flight calls.
Startup failures are bounded diagnostics and cannot prevent the ordinary
plugin-free HTTP provider from serving.

## Exact proposed owner paths

New production files:

- `pkg/extension/account_checkin.go`, `gateway_activation.go`: typed wire,
  finite validation and cloning.
- `sdk/js/account-checkin.js`, `gateway-activation.js`: matching validation.
- `internal/extension/account_checkin.go`, `checkin_intent.go`: exact leased
  action admission, durable per-account intent and terminal receipt owner.
- `internal/extension/gateway_activation.go`: revocable generation handle.
- `internal/provider/extension_checkin.go`: ordinary runner and card adapter.
- `internal/gateway/extension_activation.go`: listener-owned attachment.

Bounded existing-file seams:

- `pkg/extension/contract.go`: only `Manifest.Activation` and
  `ProviderDescriptor.Checkin` declarations; version remains with its owner.
- `pkg/extension/manifest.go`: only validator calls and deep clone additions.
- `sdk/js/manifest.js`, `index.js`, `index.d.ts`, `package.json`: declaration
  validation, exports/types and package file inclusion; no version change.
- `internal/extension/changes.go`: removable change subscription, preserving
  existing `OnChange` behavior and listener ordering.
- `internal/extension/manager.go`: post-lazy-start check-in admission and a
  joined gateway activation owner in manager shutdown; no version change.
- `internal/provider/checkin.go`: append SDK runners to `namedCheckiners`.
- `internal/provider/plugin_checkin.go`: append SDK manual/listing/marking
  projections to the ordinary plugin-facing owner, preserving legacy gates.
- `internal/provider/workbuddy_checkin.go`: optional generic operation
  receipt on a saved check-in only if needed; no vendor behavior changes.
- `internal/gateway/gateway.go`: one attachment after successful bind and
  its shutdown handle; no controller or request/accounting edits.

Add focused test siblings and a generic fixture under
`internal/extension/testdata/checkinworker`. Documentation changes belong
to this ADR, the executable/provider subsystem references and SDK README.
Any additional production path or larger existing-file change needs a
new scope message first. Shared files must be reviewed/staged by hunk,
not as unqualified whole-file ownership.

## Red gates before production

1. Capture compilable behavioral failures through ordinary
   `CheckinAccounts`, `CheckInAccount`, Settings/usage-card projection and
   the gateway activation attachment, using an actually installed generic
   exec fixture. New wire data is JSON so missing fields cannot hide
   behind a test compilation failure.
2. Prove two same-name accounts remain distinct, automatic check-in is
   default-off, account exclusion and provider Off preserve the ordinary
   manual policy, and no live network/account is involved.
3. Hold dispatch across package/config/account replacement and a network
   hold: no stale or held call reaches the fixture. Record terminal
   accepted failure, worker death and invalid result; a second timer,
   manual call and fresh manager must not issue a second write. Retain a
   definite-not-started control and completed non-claim recheck control.
4. Exercise real component Start/dispose for enabled gateway activation,
   configuration change, disable/removal and shutdown. A failed bind,
   `New`/`Front`, empty catalog and unavailable Bun must start no worker.
   Assert state/events rather than sleeping for a background timer.
5. Prove the critical terminal/retirement guard with an isolated source
   mutant. Run scoped race20, vet, build, formatting and both JS engines
   on immutable copies on `usbox`, with isolated HOME/XDG, pinned offline
   caches and native positives in the initial user namespace.

## Implementation and scoped evidence

`AccountCheckinDescriptor` is `ProviderDescriptor.Checkin` with JSON
`checkin:{action,recheck}`. No refresh/renewal field is reserved or owned by
this slice. The adapter uses the existing selected-account `binding.prepare`
and transactional credential refresh owner.

The ordinary plugin check-in enumeration appends SDK projections. Automatic
default-off Settings, provider Off, account exclusion, manual override,
Beijing owner day and quota-card projection continue through their existing
owners. SDK keys use the namespaced provider and exact account ID. Saved
results also keep the selected scope, operation ID and terminal state.
The separate private `checkins` store reserves an intent before the write;
it survives lost display records, manager restart and credential generation
replacement. Unknown/failed completion blocks subsequent writes. A proven
not-started refusal and an ordinary known non-claim recheck each reserve a
new operation ID. No inference usage row is manufactured for credit grants.

`BeginGatewayActivation` owns one handle per manager. The ordinary
`ListenAndServe` attachment is after successful bind; `New` and `Front`
remain metadata-only. In-process change notifications have a removable
subscription and external catalog/config writes are reconciled by bounded
polling. Withdrawal releases the lease and retires its cached generation;
the existing drain/disposer runs before replacement activation. Manager
shutdown cancels and joins the activation owner. Startup errors are bounded
and leave the ordinary listener serving.

All initial proof files are retained at
`usbox:/tmp/magpie-checkin-activation.1roJJ6i5/`. Initial admission red
compiled and failed: SDK 7 events, provider 5 and gateway 1, no skips. The
first provider/gateway attempt rejected unknown manifest fields. The
`red-owner/` successor includes only a wire scaffold: its actual installed
fixture still produced `CheckinAccounts=[]`, and an actual bound gateway
produced no activation event. That is the behavioral owner red, separate
from installation admission.

`probe1/` passed the first implementation at count1 (SDK 7, provider 5,
gateway 1); `probe2/` passed the expanded selected-policy, exact-owner-day,
recheck, stale-authority and lost-display controls. `probe3/` retains the
new lease tests' failures: the public primary still advertised check-in for
a stripped private action, and the close test observed the start callback
before readiness. Public Go/JS projections now omit that foreign action's
check-in metadata, while the installed root keeps it. The lifetime test
waits for the actual ready runtime before closing it. `probe4/` passed all
29 Go events, both JS engines and strict TypeScript.

Final scoped proof is
`usbox:/tmp/magpie-checkin-activation.1roJJ6i5/final1/`. Its complete
source inventory is `magpie-checkin-final1.json` in that parent directory.
The source identity is
`88f103b2355e90e6cbae2cf2289dad0cab0d4ffeb79e329da93092f4653ae88f`,
archive SHA-256
`e5c6949efb07019b818417f88d69362c952d116e27c450929d90aa11b6bde1d0`,
based on `9e88f10982a1d66c1b402fba27caddda8ac4ac4e` plus inventoried
dirty shared SDK additions. The base commit alone does not contain those
contracts. This evidence update is later documentation, not part of that
tested source fingerprint.

All four scoped commands use `go test -race -count=20 -tags nogui`,
JSON output and a 1200-second package timeout:

| Scope | Run expression | PASS / FAIL / SKIP |
| --- | --- | --- |
| `pkg/extension` | `^Test(CheckinAndActivationAdmission\|AccountCheckinWireAndClone)$` | 160 / 0 / 0 |
| `internal/extension` | `^Test(GatewayActivationCloseRetiresCachedWorker\|CheckinLeaseRechecksAfterPrivateStartup)$` | 80 / 0 / 0 |
| `internal/provider` | `^TestExtensionCheckin` | 280 / 0 / 0 |
| `internal/gateway` | `^Test(ExtensionGatewayActivation.*\|EmptyPluginCatalogsUseOrdinaryGateway)$` | 60 / 0 / 0 |

`evidence.json` records the exact commands and each log SHA-256. Total:
580 passing events, zero failures or skips. `extra-evidence.json` records
empty `gofmt -d`, 11 passing Node tests, 11 passing Bun tests, strict
TypeScript, scoped vet for Linux, Darwin and Windows, and a Linux nogui
host binary build, all exit zero. Darwin/Windows vet uses no cgo and is
cross-compilation evidence, not execution on those systems. Every run uses
temporary HOME/XDG, existing pinned offline Go caches and the initial user
namespace. No live account or vendor request is involved.

Three isolated Go overlays compiled and failed on the guarded behavior:

| Mutant | Failure | Log SHA-256 |
| --- | --- | --- |
| Bypass an unresolved durable intent | Repeated accepted write across manual/generation/restart; 4 FAIL events | `aed2c77dd38bbdd7353fdf5b31c54284510fc894a68c0fe54a90f29e2321de58` |
| Only release the activation lease | Missing disposer with manager still alive; 1 FAIL | `5d21413ee5581fc86a6d8c7d6ad5f04ad2d7af5d2eafbf84cd2ae1b9a948e5fa` |
| Remove the post-start check-in gate | Replaced configuration dispatches after lazy startup; 2 FAIL events | `946f41071241639e7a6c69d56f7f6b8780928358b4dbebbfa490fdb3ff209292` |

`negatives/evidence.json` retains source, mutant and overlay hashes. The
positive source was hash-checked unchanged after the overlays. Earlier
admission, owner-red and intermediate failed logs remain retained.

`ProviderDescriptor.KeepAlive` / `keepAlive:{leadMs}` is the separate
renewal owner's declaration. It does not overlap `Checkin`; both adapters
reuse transactional refresh. This slice neither edits its semantics nor
claims its verification. The listener-owned ledger observer attachment
also keeps its own owner beside ordinary `ServeObservers`, while activation
attaches immediately after successful `recordListening`.

System One implementation review, the CommandCode module's unfinished Jev
adapter, own-model metadata and the independent first plugin-free rollout
remain separate deliverables. No live activation, host staging, commit or
push is part of this slice.
