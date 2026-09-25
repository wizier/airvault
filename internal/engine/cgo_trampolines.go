package engine

// C trampolines forwarding native callbacks (operation progress, Rust tracing)
// to the exported Go functions. They must live in a file WITHOUT //export: cgo
// copies a //export file's preamble into two C units, so definitions would collide.

/*
#include <stddef.h>
#include <stdint.h>
#include "airvault.h"
extern void goBackupCallback(size_t callback_id, int32_t phase, double percent, uint64_t bytes);
extern void goInstallCallback(size_t callback_id, int32_t phase, uint64_t percent);
extern void goRustLogCallback(int32_t level, char* target, char* message, char* fields_json);
void av_backup_trampoline(size_t callback_id, int32_t phase, double percent, uint64_t bytes) {
	goBackupCallback(callback_id, phase, percent, bytes);
}
void av_install_trampoline(size_t callback_id, int32_t phase, uint64_t percent) {
	goInstallCallback(callback_id, phase, percent);
}
void av_log_trampoline(int32_t level, const char* target, const char* message, const char* fields_json) {
	goRustLogCallback(level, (char*)target, (char*)message, (char*)fields_json);
}
*/
import "C"
