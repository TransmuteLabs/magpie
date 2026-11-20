# SDK search sources

SDK modules contribute sources to Settings › Web search through the
`search` contribution point. The registry also projects API 1.8
`searchSources` declarations to that point. A descriptor names an id,
display name and declared primary `read` action. This registration is separate from a tool
named `web_search`. It uses `module-rpc`; it grants neither write effects
nor private services the primary module's account authority.

The public types and bounds live in
[`search_sources.go`](../../pkg/extension/search_sources.go) and
[`search-sources.js`](../../sdk/js/search-sources.js). Older API versions,
undeclared or write actions, duplicate sources and foreign providers fail
manifest validation. A source may name one provider declared by the same
module with account-auth and outbound-executor, or an API 1.11 account-only
provider with account-auth and no inference surface. Both use the same selected
account and refresh checks; neither borrows authority from an inference sibling.
A source without a provider declaration receives no account or credentials. Its configuration belongs
to the module, rather than the search owner's saved keys or URL overrides.

[`extension_search.go`](../../internal/provider/extension_search.go)
projects enabled metadata as `sdk:<module>:<source>` vendors. Listing and
selecting a source does not start a worker or Bun. The ordinary Settings
API saves, orders and switches these vendors with the existing search
APIs. Removing or disabling a module makes its saved selection unavailable.

The ordinary gateway search loop owns query masking, counts, timeouts,
account order, account switches, refusal rests, results sanitation and the
search ledger. The adapter in
[`extension_search.go`](../../internal/gateway/extension_search.go) converts
bounded SDK pages into that path. A bounded 401/402/403/429/432/433 refusal
uses the existing search account rests. Other failures stop that source's
attempt without rotating credentials. Module error details are not
forwarded to the client or ledger. This adapter does not add inference
token billing or a separate ledger sink.

The provider adapter uses the existing selected-account preflight,
transactional refresh, network hold and provider/account proxy policy.
`SearchRequest` contains only source, query, count and chosen proxy policy;
selected credentials travel in the existing scoped SDK call authority.
The caller's model provider, proxy and credentials cannot substitute for
the descriptor's declared account.

Search discovery uses the request deadline. The selected vendor keeps its
registry binding through account selection and dispatch.
[`search.go`](../../internal/extension/search.go) calls through the
binding's namespace, package revision, configuration version, enable
generation and descriptor, and rejects publication after
disable, replacement, configuration change, account replacement or
cancellation. Host callbacks recheck the same admission. Results are unary,
bounded and decoded with unknown fields rejected; pages require HTTP(S)
URLs without embedded credentials. Ordinary page cleanup and count limits
still apply.

The SpecStream community search hook motivated this surface. The synthetic
exec fixture exercises its query/count/pages shape without loading that
community package, running Bun or contacting SpecStream. This is not live
vendor or package compatibility acceptance.

Scoped verification uses `TestSearchSource*` and `TestSearchDTO*` in
`pkg/extension`, `TestSDKSearch*` in `internal/gateway` and `internal/gui`,
and `sdk/js/test/search-sources.test.js` plus
`sdk/js/test/search-source-types.ts`. Run with the repository's isolated
HOME/XDG setup, pinned offline caches and loopback-only descendant network
guard. Full repository and live module acceptance remain separate gates.
