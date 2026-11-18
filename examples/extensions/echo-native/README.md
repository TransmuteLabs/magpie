# Native Go echo

This module is a Go C-shared library loaded into the host process. It imports
the public SDK and standard library only. `init` calls `native.Register`;
the library loader does not run `main`. The SDK supplies all three ABI
exports, so an author writes no C glue.

From the repository root, with cgo and a C compiler:

```sh
python3 examples/extensions/echo-native/build.py /tmp/magpie-echo-native
python3 examples/extensions/echo-native/conformance.py /tmp/magpie-echo-native
magpie plugin sdk add /tmp/magpie-echo-native
```

The build script creates the manifest alongside `plugin.dylib` on macOS,
`plugin.so` on Linux, or `plugin.dll` on Windows. The source manifest uses
the macOS entry; the script and embedded module select the target's entry.
Cross-compilation also requires a C compiler for the target platform.
The Python conformance check loads the library into Python's own PID and
uses only a temporary, in-memory host; it does not install anything.

The four tools exercise the common SDK contract:

- `pid`, with optional `{"text":"hello"}`, returns the host PID.
- `stream`, with `{"text":"hello"}`, emits `accepted`, base64 `data` and
  `flush`, waiting for each host acknowledgement.
- `state`, with `{"key":"echo","value":{"hello":true}}`, reads then
  compare-and-swaps this module's scoped host state. Omit `value` to read.
- `wait`, with `{}`, emits `accepted` and waits for call cancellation.

The latter three tools put the call's operation ID in the acceptance receipt.
They make no network requests, read no credentials and change no global
working directory or environment.

`Register` snapshots the manifest, handler map and inbound slice once.
Handlers and codec objects remain author-supplied implementations and must
support concurrent calls and sessions. The SDK validates their advertised
methods and applies its normal frame limits, scopes, ACKs and cancellation.

Closing a session revokes its integer handle, cancels its context and
unblocks its transport without waiting for an uncooperative handler.
An already reserved host callback can finish after logical close; its
C-owned borrowed buffer remains valid until it returns. The host must keep
that callback callable and reject a revoked opaque handle. Arbitrary stuck
C calls and handlers that ignore context cannot be forcibly stopped.
Libraries remain mapped until process exit; native modules share the host's
address space and failure domain.
