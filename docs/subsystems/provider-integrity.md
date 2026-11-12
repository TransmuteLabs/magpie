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

The statusline and tray retain errors, last-known timestamps and deferred-read
notes. A stale expired window is not converted into a fresh-looking zero.
Selected-account tray-window options use the selected cards, or their union
for an in-use selection, and preserve saved kinds not present in that subset.

## MCP policy freshness

[`internal/library/mcp_run.go`](../../internal/library/mcp_run.go) checks current
Library policy before consulting a recorded command. Removing a server or an
agent's eligibility therefore revokes access through the cached run as well.
A GET that opens a stream is checked for current eligibility.

## Verification

Focused regressions are in:

- [`provider_snapshot_test.go`](../../internal/provider/provider_snapshot_test.go)
- [`remote_quotas_cache_test.go`](../../internal/provider/remote_quotas_cache_test.go)
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
