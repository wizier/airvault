package engine

// C trampoline forwarding Rust tracing records to the exported Go callback.
// It lives outside the //export file because cgo duplicates that preamble.

/*
#include <stdint.h>
extern void goRustLogCallback(int32_t level, char* target, char* message, char* fields_json);
void av_log_trampoline(int32_t level, const char* target, const char* message, const char* fields_json) {
	goRustLogCallback(level, (char*)target, (char*)message, (char*)fields_json);
}
*/
import "C"
