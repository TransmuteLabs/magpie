//go:build darwin && cgo

package proc

/*
#include <mach/mach.h>

static uint64_t self_rss(void) {
	mach_task_basic_info_data_t info;
	mach_msg_type_number_t count = MACH_TASK_BASIC_INFO_COUNT;
	if (task_info(mach_task_self(), MACH_TASK_BASIC_INFO, (task_info_t)&info, &count) != KERN_SUCCESS) {
		return 0;
	}
	return info.resident_size;
}
*/
import "C"

// SelfRSS returns this process's resident memory in bytes.
func SelfRSS() (int64, bool) {
	size := int64(C.self_rss())
	return size, size > 0
}
