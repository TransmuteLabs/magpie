# ADR 0001: Explicit immutable protocol registries in the gateway

Date: 2026-11-12

Status: implemented as an experimental in-process Go inbound contract.

## Problem

An alternate inbound wire format needs a boundary for decoding and encoding
without acquiring gateway authority or adding a vendor to core dispatch.
Provider fetch plugins and request middleware already have different owners;
they do not register arbitrary inbound protocols. Upstream merges should keep
their existing APIs, constructors, caller guards and policy paths.

## Decision

Keep a public, standard-library-only SDK in `pkg/protocolext`. Register exact
adapter routes in an immutable registry, freezing version, capability and
identity declarations. The application's composition root supplies that registry to
`gateway.NewWithProtocols`; the existing `New()` constructor enables none.
An instance of `internal/protocolext.Host` validates route ownership and
provides exact lookup. There is no global handler registration or vendor
selector.

The gateway stays an internal package. External packages can implement the
public interface, while a composition root permitted by Go's internal import
rules must deliberately link and install them. This is a constructor seam,
not a runtime plugin-loading command.

Keep integration in `protocol_extensions.go`, with only a server field and a
handler entry hook in `gateway.go`. Normalize inside the existing caller and
read-only guards, before budget and concurrency checks. Preserve peer,
authenticated context, session and forwarding provenance, resolve gateway-key
aliases, then call the canonical handler directly. Only Chat and explicitly
negotiated authorized Models discovery are implemented. Decoding grants no
right to choose an administrative target, destination URL, caller or account.

Reject whole registration on version, required-capability or path conflicts.
Reserve existing administrative and SDK namespaces before serving. Expose a
per-server matcher for an embedding web server. GUI one-port forwarding uses
the serving instance's matcher and front handler, while the global matcher
remains static. The web proxy to another process cannot discover an arbitrary
registry today.

Adapters are trusted linked Go implementations. Their request state belongs
in the response instance and their canonical writes are synchronous. Immutable
registration freezes declarations; it does not sandbox executable Go code.

## Current implementation limits

There are no bundled vendor implementations or custom authentication/session
migrations in this port. No environment variable installs an adapter. The
public capability vocabulary also names outbound execution, wiring, auth,
usage, operation control and tools, but this host refuses them.

External Go executables and Bun modules are future work. There is no IPC
transport, supervisor, reload/drain service or cross-language implementation
behind the SDK names. Their eventual design needs explicit negotiation,
bounded streaming, cancellation, process-death handling, account/session
isolation and acceptance/replay-safety contracts before any execution path can
be described as supported. This change provides no stub success for them.

## Alternatives rejected

- Vendor switches in the kernel: each integration couples upstream merges to
  another implementation and makes route ownership implicit.
- Process-global mutable handlers: servers and tests cannot isolate their
  registrations or freeze their negotiated authority.
- Intercepting existing core endpoints: bypasses route-specific administrative
  and SDK policy and risks shadowing the page's API.
- An internal loopback HTTP client: loses provenance, introduces a second
  request and obscures the canonical policy ordering.
- Implementing a partial process runtime in this slice: implies guarantees
  that cannot be tested without the complete execution and lifecycle contract.

## Verification

Use the dummy-adapter tests and isolated commands in the
[subsystem reference](../subsystems/protocol-extensions.md). They exercise the
generic contracts and real gateway front stack. Vendor protocol parity,
external runtimes and web registration discovery require separate evidence.
Repository-wide acceptance checks in the code standards still apply before
merge or release.
