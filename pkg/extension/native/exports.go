//go:build cgo

package native

/*
#include "abi.h"
#include <stdlib.h>

static inline int32_t magpie_native_call_emit(
    magpie_native_emit_v1_fn emit, uintptr_t host,
    const uint8_t *data, size_t length) {
    return emit(host, data, length);
}
*/
import "C"

import (
	"unsafe"

	sdk "github.com/yetone/magpie/pkg/extension"
)

const available = true

//export magpie_extension_open_v1
func magpie_extension_open_v1(host *C.magpie_native_host_v1, handle *C.uint64_t) (status C.int32_t) {
	status = 1
	defer func() {
		if recover() != nil {
			status = 1
		}
	}()
	if handle == nil {
		return
	}
	*handle = 0
	if host == nil || host.abi_version != C.MAGPIE_NATIVE_ABI_V1 ||
		host.struct_size < C.sizeof_magpie_native_host_v1 {
		return
	}
	callback, opaque := host.emit, host.host
	if callback == nil {
		return
	}
	id, err := library.open(func(body []byte) error {
		data := C.CBytes(body)
		defer C.free(data)
		if C.magpie_native_call_emit(callback, opaque, (*C.uint8_t)(data), C.size_t(len(body))) != 0 {
			return errCallback
		}
		return nil
	})
	if err != nil {
		return
	}
	*handle = C.uint64_t(id)
	return 0
}

//export magpie_extension_send_v1
func magpie_extension_send_v1(handle C.uint64_t, data *C.uint8_t, length C.size_t) (status C.int32_t) {
	status = 1
	defer func() {
		if recover() != nil {
			status = 1
		}
	}()
	if length > sdk.MaxFrameBytes+1 || data == nil && length != 0 {
		return
	}
	body := unsafe.Slice((*byte)(unsafe.Pointer(data)), int(length))
	if library.send(uint64(handle), body) != nil {
		return
	}
	return 0
}

//export magpie_extension_close_v1
func magpie_extension_close_v1(handle C.uint64_t) (status C.int32_t) {
	status = 1
	defer func() {
		if recover() != nil {
			status = 1
		}
	}()
	if library.close(uint64(handle)) != nil {
		return
	}
	return 0
}
