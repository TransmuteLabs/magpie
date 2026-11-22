# Provider integrity and current policy

These are vendor-neutral core guarantees retained during the SDK split.
Provider/account ownership remains documented in
[Providers and accounts](providers-accounts.md); plugin execution remains in
[Provider and plugin ownership](provider-plugins.md).

## Checked writes

[`internal/provider/provider.go`](../../internal/provider/provider.go) captures
the source fingerprint when a provider is read. Publishing an edit compares
that source under the existing write lock. A stale or source-less replacement
of an existing record is refused instead of overwriting another writer.
Callers load a fresh provider before editing; creating a new id remains a
separate operation.

[`rename.go`](../../internal/provider/rename.go) moves a provider's catalog
only after its configuration change has been published successfully.
Notifications follow those moves. A rejected configuration write therefore
does not rename catalog files.

## Remote quota cache

[`remote_quotas.go`](../../internal/provider/remote_quotas.go) includes an
endpoint digest in its cache identity. Changing a remote provider's address
cannot reuse the previous endpoint's cards under the same local provider id.
Its successful-read timestamp is separate from failed refresh attempts:
carried cards remain visibly stale rather than becoming fresh on failure.
Returned windows and nested reset lists are copied before caller mutation.
Canceling a caller's read leaves the remote cache eligible for an immediate
retry. A remote timeout or refusal observed while the caller is still active
keeps its usual cache and freshness semantics.

The gateway gives quota scope selection, account discovery, card refresh and
projection the same request deadline. It sends a gateway timeout when its
own deadline expires with the client still connected. An expired request
does not publish partial account or quota data.

The statusline and tray retain errors, last-known timestamps and deferred-read
notes. A stale expired window is not converted into a fresh-looking zero.
Selected-account tray-window options use the selected cards, or their union
for an in-use selection, and preserve saved kinds not present in that subset.

## Local allowance snapshots

The account editor, Usage page, quota reports, and `magpie accounts`
check the assembled subscription readings before publishing them. They
compare the current SDK receipt and subscription owner, including legacy
readings without an SDK receipt. A revoked reading becomes a deferred card.
Other accounts retain their reading, and remote cards and key-only cards
keep their own authority. This check uses local state and never refreshes
the vendor.

Each publication check reads the catalog and subscription owners once per
SDK host. It reads one account file and configuration per selected module,
under the catalog and Store read locks. Every account must still match its
own generation, usage revision, and owner revision, including an initially
unowned subscription. The package, configuration, and enablement generation
must match too. Scope selection and history writers take a fresh snapshot
after their own waits.

Routing binds cached allowance windows and read timestamps to each
account's sign-in. For a Community plugin it checks the saved slot,
account identity, and the credential lifetime committed by explicit sign-in.
The publication reads one current credential projection for the batch.
When routing has cached Community allowances, it uses one current owner
snapshot both to validate those allowances and to resolve an SDK cutover.
After waiting for the first reading it checks the current owner again.
A replacement in another process defers the old reading even when
`logins.json` could not be updated. Usage's plan, sign-in status and wallet
writes hold the credential lifetime through publication. Token renewal
preserves that lifetime, and an older saved slot without an explicit
lifetime remains valid until its next sign-in. The same check applies when
routing falls back to the last saved reading.

[`quota_last.go`](../../internal/provider/quota_last.go) keeps the latest
successful account reading for transient usage failures. A fresh reading
replaces the in-memory snapshot with its exact model matcher and read sequence,
so a later failure or older response cannot widen that model's hold to its
siblings.

Function-backed matchers cannot be serialized. Their account entries stay in
memory and are omitted from the next successful disk publication, which also
removes an older serializable entry for that account. Unrelated serializable
accounts remain cached. Disk persistence is best effort: a failed write does
not establish durable invalidation across a restart.

Local quota, balance and daily credit histories check whether a new
subscription reading still belongs to the current owner after acquiring
their history writer lock. The account editor and Usage page use the same
owner check for quota history.
A revoked SDK acknowledgment or a legacy reading overtaken by a transfer
adds no point and cannot advance the last note time. The check reads local
state and makes no vendor request. Plan and balance history for API keys
remain independent of subscription ownership.

Claude account renewal keeps a confirmed sign-in result. Its quota history
and kept card follow the caller's deadline.

[`credits_daily.go`](../../internal/provider/credits_daily.go) reads the
latest history and writes its updated totals under `credits-daily.json.lock`.
Each window keeps the start time of its last recorded Usage read, even when
the figures stayed the same. A reply from an older read and a replay of
the same cached reading leave that window's history intact. A newer drop
still starts a cycle, and histories saved before read ordering supply
their previous figures as the starting point. A refresh with no fresh
credit windows does not take the lock. A canceled wait leaves the history
unchanged, and the reader still shares its parse until the file changes.

Quota reports and statusline retain their subscription receipts through
last-served lookups and gateway-key account scoping. The report checks the
selected readings before projecting the output. A held key sees the
allowed accounts and their windows without the account-wide usage totals.

Community subscription readings name the selected login ID, slot, and
sign-in lifetime. The account editor, quota cache, and export match that
selection before reusing a reading, including ordinary allowance windows
and balance-only responses. Explicit sign-in advances the lifetime. A
temporary vendor failure can reuse a reading of the same sign-in. A
replaced sign-in gets a fresh read or a deferred card with its identity.
Earlier cache records without a selection can serve an existing login
whose saved lifetime is still unset.

## Wallet spend

[`wallet_spend.go`](../../internal/provider/wallet_spend.go) reads the
current counters under the same file lock that protects their replacement.
Each process records all windows from a Usage reply together with its
observation order. The order compares the saved login revision seen at
dispatch and the read's start time within that revision. A newer reading
keeps its place even when the balance is the same. A late reply from the
same account lifetime is deferred once a newer reading has been recorded.
Counters saved before observation ordering remain valid starting balances.
A canceled observation leaves the saved counters intact. An unreadable
snapshot gives an error and keeps the original file.

[`pluginLoginQuota`](../../internal/provider/plugin_usage.go) checks community
subscription ownership after receiving the reply and holds it through the
wallet write. Saved accounts also check the login ID and account lifetime
under the login lock. The locks are acquired in ownership, login, then
wallet order. The wallet reader checks the current discovery setting after
waiting for the login and wallet locks and before replacing the file. Public
keyless providers can still record counters with discovery off. A rejected
wallet reply leaves the original counters intact. The account editor and
quota export show the selected account's identity and the reason the reading
was deferred. Cached readings remain available for ordinary transient vendor
errors.

Login checks that authorize quota publication, an account task, or a saved
sign-in transfer read the current `logins.json` bytes through `filememo.ReadCurrent`.
A matching digest reuses the parsed accounts. Replacing the file while
preserving its size and modification time still changes the next check, and
an invalid current file defers the reading even if a backup can restore the
account list for display.

Strict account-discovery and legacy LAN key checks share the primary settings
reader in [`current.go`](../../internal/settings/current.go). It checks the
current bounded file on each read and reuses validated settings when the
contents match. Strict reads and saves share a publication lock within the
process. The lock covers the write itself, so checks can still read settings
while another operation holds the broader settings transaction lock. An
observed file replacement can be retried up to two times. An absent primary
on the initial lookup gives the defaults. Blank, null, malformed or
unreadable primary files remain errors even with a recoverable backup.
`LoadCurrent` gives callers an owned copy.

## MCP policy freshness

[`internal/library/mcp_run.go`](../../internal/library/mcp_run.go) checks current
Library policy before consulting a recorded command. Removing a server or an
agent's eligibility therefore revokes access through the cached run as well.
A GET that opens a stream is checked for current eligibility.

## Verification

Focused regressions are in:

- [`provider_snapshot_test.go`](../../internal/provider/provider_snapshot_test.go)
- [`remote_quotas_cache_test.go`](../../internal/provider/remote_quotas_cache_test.go)
- [`quota_last_scope_test.go`](../../internal/provider/quota_last_scope_test.go)
- [`wallet_spend_publication_test.go`](../../internal/provider/wallet_spend_publication_test.go)
- [`statusline_test.go`](../../internal/provider/statusline_test.go)
- [`mcp_run_test.go`](../../internal/library/mcp_run_test.go)
- [`dsh_max_effort_test.go`](../../internal/agent/dsh_max_effort_test.go)
- [`trayusage_test.go`](../../internal/gui/trayusage_test.go)
- [`tray-windows.test.cjs`](../../internal/gui/tests/tray-windows.test.cjs)

Run Go checks in a temporary HOME with pinned caches as specified by the
[code standards](../code-standards.md). Browser checks use the existing
Playwright installation and cover Chromium, WebKit and the five GUI languages.
These contracts do not introduce vendor-specific reset operations, a partial
quota schema or account-bound MCP credentials.
