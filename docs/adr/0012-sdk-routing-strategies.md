# SDK routing strategies at the ordinary seat-ordering point

Date: 2026-11-13
Status: Implemented with scoped verification and independent review; combined SDK checkpoint passed.

## Decision

SDK API 1.4 root modules may declare `routingStrategies`. Each descriptor
names an existing declared **read** action and requires `module-rpc`.
The provider or group selects it through its ordinary `routing` setting,
using the fixed name `moduleID/strategyID`.

The contract extends the existing
[`routes.Strategy`](../../internal/routes/routes.go) ordering point.
It does not grant account selection, credentials, retry authority, arbitrary
gateway endpoints or control over candidate admission. Smart and directly
registered strategies retain priority. Bare builtin names never enter SDK
lookup; a module's descriptor cannot name another module's namespace.

## Author contract

The following fragment is used by the tested `routing.fixture` module.
Its root manifest also declares schema, package version, runtime and entry.

```json
{
  "contract": {
    "version": {"major": 1, "minor": 4},
    "capabilities": ["module-rpc"]
  },
  "actions": [
    {"id": "rank", "effect": "read", "inputSchema": {"type": "object"}}
  ],
  "routingStrategies": [
    {
      "id": "reverse",
      "name": "Reverse fixture",
      "action": "rank",
      "flags": {"exact": true},
      "timeoutMs": 2000
    }
  ]
}
```

The action receives a
[`RoutingRequest`](../../pkg/extension/routing.go): `strategy` is the local
descriptor ID, `pool` identifies the current pool, and `seats` contains its
bounded metric projection. Return one unary `RoutingResult` with `order`
containing every seat index exactly once. The
[Go worker fixture](../../internal/extension/testdata/routingworker/main.go)
implements a real handler. The
[TypeScript consumer](../../sdk/js/test/routing-types.ts) checks the public
types; JS exports `validateRoutingRequest` and `validateRoutingResult`.

The read action may belong to the primary or to a declared private service.
The full installed root retains both contributions. Each component's public
handshake exposes only routing descriptors whose actions it owns, matching
its filtered action list. This projection does not remove the root's
registration: Manager resolves the full root binding and the component group
starts the private action's helper lazily on the selected generation.

`Exact`, `ByUse`, `ByFree`, `ByPlan`, `BySpeed` and `ByPrice` retain their
existing Strategy meanings. Metric flags request the corresponding core
measurements. `Exact` retains the strategy's order through the point's
secondary fit sorting; it grants no permission to admit a barred candidate.

The descriptor name is displayed in the existing routing selector.
For this fixture, save `routing: "routing.fixture/reverse"` on the provider
or group. A missing or disabled strategy falls back to listed order.

Bounds are shared by Go and JS:

| Value | Bound |
| --- | --- |
| Strategies per root module | 64 |
| Seats per ordering request | 1–256 |
| Renewal timestamps per seat | 32 |
| Routing request/result JSON | 1 MiB each |
| Weight | 1–1,000,000 |
| Numeric measurements | Finite, absolute value at most 10^15 |
| Unix millisecond timestamps | JavaScript safe integer range |
| Ordering context | Default 500 ms; explicit 25–2000 ms |

Seat and wallet keys are opaque hashes. Requests carry no raw API key,
email, selected-account binding, auth or conversation content. The action
gets module scope only. API 1.4 read authority refuses persistent
`state.cas` mutations, including attempted callbacks from the real worker.

## Host boundary and lifecycle

[`routes.Find`](../../internal/routes/routes.go) tries lazy lookup only
after its registered lookup misses, with the registry lock released.
[`middleware.Routings`](../../internal/middleware/points.go) appends current
SDK metadata to the existing selector's entries.

The package boundary avoids an existing dependency cycle:

- [`extension.Manager`](../../internal/extension/routing.go) owns SDK-only
  catalog bindings, selected revision acquisition, RPC and result validation.
- [`extensionrouting`](../../internal/extensionrouting/routing.go) projects
  core seats and adapts a binding to `routes.Strategy`.
- [`gateway`](../../internal/gateway/extension_routing.go) registers lazy
  function pointers. Registration performs no catalog read or worker startup.

Metadata reads reflect the current enabled catalog. Pure discovery, List
and synctest do not start workers, lifecycle channels, timers or warmers.
Ordering acquires one selected package/configuration lease, verifies its
descriptor and rechecks availability, revision and configuration after the
result. Disable, replacement or configuration changes invalidate held
results; another process's catalog changes are visible.

Invalid output, streamed action events, errors, deadlines and revoked results
return identity order. Every candidate already admitted by the core remains
available. Account/model/key guards, rests, affinity and failover stay with
their existing owners.

The ordering context deadline includes cold acquisition and the RPC.
The upstream `Strategy.Order` signature has no HTTP request context, so this
adapter uses a bounded background context and does not propagate HTTP caller
cancellation through that seam. Nested ordering points have separate
deadlines. These bounds are a failure policy, not a latency benchmark or a
guarantee that cooperative native code can be forcibly interrupted.

## Verification

The immutable source is
`usbox:/tmp/magpie-routing14-final-R4VXD33a/source`; source archive SHA256:

```text
bbfba140c55787efc42d60d7b979ea75f175a91795eef1fea822ba974eaf58a0
```

Adjacent `race-results.json`, `static-results.json`, `js-results.json`,
`negative-results.json` and `gofmt-result.json` record terminal commands.
Tests use isolated HOME/XDG, pinned offline Go caches and synthetic servers.

| Gate | Result |
| --- | --- |
| Scoped SDK/routes/adapter/Manager/gateway routing tests, race ×20 | Five packages, exit 0 |
| Ordinary HTTP provider and group routing | Real Go worker; expected second key/member selected |
| Core guards and fallback | Off key excluded; caller model hold reaches no inference; invalid/error/timeout preserves listed order |
| Catalog and lease lifecycle | Off/on, package replacement, config change and held cross-process disable |
| Pure metadata | Synctest and fresh Manager stay cold |
| Full JS suite on this snapshot | Node 114 pass; Bun 114 pass; no failures or skips |
| Strict TypeScript | Five consumer fixtures, exit 0 |
| Scoped Linux vet; full `nogui` build | Exit 0 |
| gofmt | Fifteen changed Go files clean |

Three negative overlays produce actual behavioral assertion failures:
forcing identity returns key-a where the ordinary request requires key-b;
removing permutation validation accepts a duplicate index; removing final
revocation/config checks accepts held stale results. None is a compilation
failure or skipped test.

This record proves a real exec worker path. It does not add a native C-ABI
routing runtime proof, live vendor proof, whole-repository green result or
combined SDK 1.4 acceptance. The
[API 1.3 checkpoint](0009-sdk13-validation.md) remains the previous combined
record; the [surface census](../subsystems/extension-surface-coverage.md)
continues to list unimplemented adapters.

### Hybrid projection regression and fix

The original projection retained private-service routing descriptors after
removing their actions. A root accepted by validation then failed its primary
handshake. New Go/JS projection tests and an actual Manager lazy-helper test
fail on the original code with undeclared read-action errors. The fix filters
component public routing metadata together with its owned actions; the full
root, catalog registration and private dispatch authority remain intact.

The fix snapshot is
`usbox:/tmp/magpie-routing14-hybrid-green-zB0ZeHhw/source`, based on the
immutable source above plus the two projection fixes and four regression
test/fixture files. Its archive SHA256 is:

```text
578030c9bd90495e83653546f5e55611836ec37a6c66af3e1c4184625bd5ff76
```

Scoped routing/composite projection tests across the same five packages
passed race ×20. Node and Bun each passed 115 tests without skips; five
strict TS fixtures, scoped Linux vet, full `nogui` build and formatting of
the four changed Go files passed. Adjacent `green-results.json`,
`js-results.json` and `static-results.json` record the terminal commands.

The actual helper fixture proves that primary acquisition does not start the
lazy backend, routing starts that root's configured backend, the helper
cannot write host state from its read action, and a forged private action
binding is rejected before another dispatch. Full-root registration remains
discoverable although the primary's public routing list is empty.

The red fixture source is
`usbox:/tmp/magpie-routing14-hybrid-red-EMNBCTSX/source`, archive SHA256
`7b5a63fb92019c953865a9ac92bce6267e48a0229dfb51a401954e86e8a6aeb0`;
it differs from the original snapshot only by regression tests/fixture
changes. Both Go tests and the JS projection test fail with behavioral
assertions, without compilation errors. These additional scoped gates still
do not establish combined SDK 1.4 acceptance.
