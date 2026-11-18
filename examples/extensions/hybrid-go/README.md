# Hybrid Go SDK example

This offline distribution combines a Go C-shared controller, a separately
compiled Go service and a static TypeScript page. It uses SDK API 1.1 and
native ABI 1, as specified in [ADR 0005](../../../docs/adr/0005-composite-module-contract.md).
The primary registers through `pkg/extension/native`; authors write no C glue.
The handler's PID is the Magpie host PID. The backend has its own process.

Build with Go, a C compiler, Python 3 and an already installed `tsc`:

```sh
GOPROXY=off GOSUMDB=off python3 examples/extensions/hybrid-go/build.py /tmp/hybrid-v1
GOPROXY=off GOSUMDB=off python3 examples/extensions/hybrid-go/build.py /tmp/hybrid-v2 --version 2.0.0
magpie plugin sdk add /tmp/hybrid-v1
magpie plugin sdk call sdk-hybrid-go module.call '{"action":"pid","input":{}}'
magpie plugin sdk call sdk-hybrid-go module.call '{"action":"forward","input":{"text":"hello"}}'
```

Use a fresh, empty distribution directory. The build copies only runtime
payloads and writes a descriptor for the target platform. The same Go linker
version override applies to both controller and service. TypeScript compiles
at author time; installation and execution need neither Bun nor an npm install.

The root owns four unary actions:

| Action | Component | Effect |
| --- | --- | --- |
| `pid` | native primary | read |
| `forward` | native primary | write |
| `backend.pid` | external backend | read |
| `backend.run` | external backend | write |

`forward` uses `Stream.HostCall(component.call)` with the declared
`controller.run` binding. The backend writes `hybrid.last` through the
parent's scoped state callback. Identity results contain the module version,
configuration version/value, component instance and PID. When a synthetic
account is selected they include its ID/generation and a credential digest,
never raw credentials.
`backend.run` also returns the domain field `"outcome":"accepted"`. This is
ordinary action data: the terminal unary result completes the root journal;
it is not an inference execution outcome or an unfinished operation.

`stream`, `wait` and `forward` are primary tool fixtures. `forward` emits
`before-forward` as a data event before its host callback. A test can hold
that event's acknowledgement, update the catalog/configuration, and then
prove that the request lazily starts the service from its original group.
`module.call` itself remains unary.

The lazy backend's `Module.Start` binds `127.0.0.1:0` and publishes endpoint
`ui` through SDK readiness. `/status` returns its frozen configuration and
identity. `/events` sends an initial SSE readiness event and remains open
until canceled. Every HTTP response receives fresh `ModuleContext.HTTPHeaders()`
before its status or body: these bind the HTTP peer to its RPC instance,
component and configuration version. The GUI proxy validates and removes those
proof headers before forwarding the response to the window.
The module lifetime and disposer close the listener and
active HTTP streams. Configuration `{"failStart":true}` deliberately fails
after opening the listener and returns its disposer to exercise rollback.
The default configuration is `{}`; `{"value":"example"}` makes shared
configuration visible in both components.

The fake `hybrid-fixture` provider advertises a synthetic model. Its auth
finish accepts only `{"id":"account-a"}` or `{"id":"account-b"}` and issues
offline fixture credentials. It implements no vendor networking or inference.

`package.json.magpie.page` points at built static assets. The page invokes
only declared `page.*` bindings through the parent bridge; it contains no
backend URL, account selector or credentials. Browser/native-webview
verification belongs to the GUI adapter tests and is not implied by building
this example or by the Manager integration tests.
