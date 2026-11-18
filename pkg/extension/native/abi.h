#ifndef MAGPIE_EXTENSION_NATIVE_ABI_H
#define MAGPIE_EXTENSION_NATIVE_ABI_H

#include <stddef.h>
#include <stdint.h>

#define MAGPIE_NATIVE_ABI_V1 1u

/*
 * Versioned C ABI for an in-process extension, independent of Go's plugin ABI.
 * Each byte span is borrowed only until the call returns. Copy it before
 * retaining it. No Go pointer, Go interface, allocator-owned buffer, or
 * C++ object crosses this boundary.
 *
 * Data contains the same bounded SDK v1 JSON frames used by the JS host.
 * Their envelope does not require an external process or an OS socket.
 * A return value of zero means success; all other values mean failure.
 */
typedef int32_t (*magpie_native_emit_v1_fn)(
    uintptr_t host, const uint8_t *data, size_t length);

typedef struct {
    uint32_t abi_version;
    uint32_t struct_size;
    uintptr_t host;
    magpie_native_emit_v1_fn emit;
} magpie_native_host_v1;

/*
 * A library exports these symbols:
 *   magpie_extension_open_v1
 *   magpie_extension_send_v1
 *   magpie_extension_close_v1
 *
 * open copies the host descriptor and returns an opaque session ID. It must
 * not emit before the first send. send copies/consumes one input byte span.
 * Calls for different sessions may be concurrent; a host serializes sends
 * within a session. emit may run on another thread and supplies bounded
 * bytes synchronously, preserving host backpressure.
 *
 * close revokes a session, cancels its handlers and stops future callbacks.
 * It must unblock pending send/emit calls and return promptly. It cannot
 * promise to undo vendor effects. A native extension is trusted host code:
 * the host cannot kill a stuck C call or isolate a process-wide crash.
 *
 * Libraries remain mapped for the lifetime of the host process. Logical
 * session close is separate from unloading code with a Go runtime.
 */
typedef int32_t (*magpie_native_open_v1_fn)(
    const magpie_native_host_v1 *host, uint64_t *session);
typedef int32_t (*magpie_native_send_v1_fn)(
    uint64_t session, const uint8_t *data, size_t length);
typedef int32_t (*magpie_native_close_v1_fn)(uint64_t session);

#endif
