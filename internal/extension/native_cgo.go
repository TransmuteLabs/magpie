//go:build cgo && (darwin || linux || freebsd || windows)

package extension

/*
#cgo linux LDFLAGS: -ldl
#cgo freebsd LDFLAGS: -ldl
#include "../../pkg/extension/native/abi.h"
#include <stdio.h>
#include <stdlib.h>

#ifdef _WIN32
#include <windows.h>
#else
#include <dlfcn.h>
#endif

typedef struct {
	void *handle;
	magpie_native_open_v1_fn open;
	magpie_native_send_v1_fn send;
	magpie_native_close_v1_fn close;
} magpie_native_image;

extern int32_t magpieNativeHostEmit(uintptr_t host, uint8_t *data, size_t length);

static int32_t magpie_native_emit_bridge(uintptr_t host, const uint8_t *data, size_t length) {
	return magpieNativeHostEmit(host, (uint8_t *)data, length);
}

static int magpie_native_load(const char *filename, magpie_native_image *image, char *error, size_t capacity) {
#ifdef _WIN32
	int count = MultiByteToWideChar(CP_UTF8, MB_ERR_INVALID_CHARS, filename, -1, NULL, 0);
	if (count == 0) {
		snprintf(error, capacity, "native library filename conversion failed (%lu)", (unsigned long)GetLastError());
		return 1;
	}
	WCHAR *wide = (WCHAR *)malloc((size_t)count * sizeof(WCHAR));
	if (wide == NULL) {
		snprintf(error, capacity, "native library filename allocation failed");
		return 1;
	}
	if (MultiByteToWideChar(CP_UTF8, MB_ERR_INVALID_CHARS, filename, -1, wide, count) == 0) {
		free(wide);
		snprintf(error, capacity, "native library filename conversion failed (%lu)", (unsigned long)GetLastError());
		return 1;
	}
	HMODULE handle = LoadLibraryExW(wide, NULL, LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR | LOAD_LIBRARY_SEARCH_DEFAULT_DIRS);
	free(wide);
	if (handle == NULL) {
		snprintf(error, capacity, "load native library failed (%lu)", (unsigned long)GetLastError());
		return 1;
	}
	image->handle = (void *)handle;
	image->open = (magpie_native_open_v1_fn)GetProcAddress(handle, "magpie_extension_open_v1");
	image->send = (magpie_native_send_v1_fn)GetProcAddress(handle, "magpie_extension_send_v1");
	image->close = (magpie_native_close_v1_fn)GetProcAddress(handle, "magpie_extension_close_v1");
#else
	void *handle = dlopen(filename, RTLD_NOW | RTLD_LOCAL);
	if (handle == NULL) {
		const char *detail = dlerror();
		snprintf(error, capacity, "load native library failed: %s", detail == NULL ? "unknown loader error" : detail);
		return 1;
	}
	image->handle = handle;
	image->open = (magpie_native_open_v1_fn)dlsym(handle, "magpie_extension_open_v1");
	image->send = (magpie_native_send_v1_fn)dlsym(handle, "magpie_extension_send_v1");
	image->close = (magpie_native_close_v1_fn)dlsym(handle, "magpie_extension_close_v1");
#endif
	if (image->open == NULL || image->send == NULL || image->close == NULL) {
		snprintf(error, capacity, "native library requires magpie_extension_open_v1, magpie_extension_send_v1 and magpie_extension_close_v1");
		return 1;
	}
	return 0;
}

static int32_t magpie_native_open(magpie_native_image *image, uintptr_t host, uint64_t *session) {
	magpie_native_host_v1 descriptor;
	descriptor.abi_version = MAGPIE_NATIVE_ABI_V1;
	descriptor.struct_size = sizeof(descriptor);
	descriptor.host = host;
	descriptor.emit = magpie_native_emit_bridge;
	return image->open(&descriptor, session);
}

static int32_t magpie_native_send(magpie_native_image *image, uint64_t session, const uint8_t *data, size_t length) {
	return image->send(session, data, length);
}

static int32_t magpie_native_close(magpie_native_image *image, uint64_t session) {
	return image->close(session);
}
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

func loadNativeLibrary(filename string) (*nativeLibrary, error) {
	name := C.CString(filename)
	defer C.free(unsafe.Pointer(name))
	var image C.magpie_native_image
	var detail [1024]C.char
	if C.magpie_native_load(name, &image, &detail[0], C.size_t(len(detail))) != 0 {
		return nil, errors.New(C.GoString(&detail[0]))
	}
	return &nativeLibrary{
		open: func(host uintptr) (uint64, error) {
			var session C.uint64_t
			status := C.magpie_native_open(&image, C.uintptr_t(host), &session)
			if status != 0 {
				return uint64(session), fmt.Errorf("native extension open failed with status %d", int32(status))
			}
			return uint64(session), nil
		},
		send: func(session uint64, data []byte) error {
			body := C.CBytes(data)
			defer C.free(body)
			status := C.magpie_native_send(&image, C.uint64_t(session), (*C.uint8_t)(body), C.size_t(len(data)))
			if status != 0 {
				return fmt.Errorf("native extension send failed with status %d", int32(status))
			}
			return nil
		},
		close: func(session uint64) error {
			status := C.magpie_native_close(&image, C.uint64_t(session))
			if status != 0 {
				return fmt.Errorf("native extension close failed with status %d", int32(status))
			}
			return nil
		},
	}, nil
}

//export magpieNativeHostEmit
func magpieNativeHostEmit(host C.uintptr_t, data *C.uint8_t, length C.size_t) (status C.int32_t) {
	status = 1
	defer func() {
		if recover() != nil {
			status = 1
		}
	}()
	if data == nil && length != 0 {
		return
	}
	if err := nativeEmit(uintptr(host), uint64(length), func() []byte {
		return C.GoBytes(unsafe.Pointer(data), C.int(length))
	}); err != nil {
		return
	}
	return 0
}
