package engine

// C trampolines forwarding operation progress callbacks to the
// exported Go functions. They must live in a file WITHOUT //export: cgo copies
// a //export file's preamble into two C units, so definitions there would collide.

/*
#include <stddef.h>
#include <stdint.h>
#include "airvault.h"
extern void goBackupCallback(size_t callback_id, int32_t phase, double percent, uint64_t done, uint64_t total);
extern void goInstallCallback(size_t callback_id, int32_t phase, uint64_t percent);
void av_backup_trampoline(size_t callback_id, int32_t phase, double percent, uint64_t done, uint64_t total) {
	goBackupCallback(callback_id, phase, percent, done, total);
}
void av_install_trampoline(size_t callback_id, int32_t phase, uint64_t percent) {
	goInstallCallback(callback_id, phase, percent);
}
*/
import "C"
