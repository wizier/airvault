package engine

/*
#include <stdlib.h>
#include "airvault.h"

void av_log_trampoline(int32_t level, const char* target, const char* message, const char* fields_json);
*/
import "C"

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	airlog "github.com/wizier/airvault/internal/logging"
)

type rustLogRecord struct {
	level                slog.Level
	target, message      string
	structuredFieldsJSON string
}

var rustLogging struct {
	sync.Once
	ok      bool
	records chan rustLogRecord
}

// InitLogging installs the one process-wide Rust tracing bridge. It must run
// before New because Rust tracing, unlike Engine configuration, is global.
func InitLogging(level string) bool {
	rustLogging.Do(func() {
		rustLogging.records = make(chan rustLogRecord, 256)
		go func() {
			for record := range rustLogging.records {
				callHost("Rust log", func() { writeRustLog(record) })
			}
		}()
		clevel := C.CString(level)
		defer C.free(unsafe.Pointer(clevel))
		rustLogging.ok = C.av_log_init(C.av_log_cb(C.av_log_trampoline), clevel) == 0
	})
	return rustLogging.ok
}

//export goRustLogCallback
func goRustLogCallback(level C.int32_t, target, message, fieldsJSON *C.char) {
	record := rustLogRecord{
		level:                slog.Level(level),
		target:               C.GoString(target),
		message:              C.GoString(message),
		structuredFieldsJSON: C.GoString(fieldsJSON),
	}
	select {
	case rustLogging.records <- record:
	default: // diagnostics must never stall a device protocol thread
	}
}

func writeRustLog(record rustLogRecord) {
	attrs := decodeRustLogAttrs(record.structuredFieldsJSON)
	if name := strings.TrimPrefix(record.target, "airvault_shim::"); name != "" {
		attrs = append([]slog.Attr{slog.String("target", name)}, attrs...)
	}
	airlog.Component("engine.rust").LogAttrs(
		context.Background(), record.level, record.message, attrs...,
	)
}

func decodeRustLogAttrs(raw string) []slog.Attr {
	if raw == "" || raw == "{}" {
		return nil
	}
	var fields map[string]any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&fields); err != nil {
		return []slog.Attr{slog.String("rust_fields", raw)}
	}
	attrs := make([]slog.Attr, 0, len(fields))
	for _, key := range slices.Sorted(maps.Keys(fields)) {
		attrs = append(attrs, rustLogAttr(key, fields[key]))
	}
	return attrs
}

func rustLogAttr(key string, value any) slog.Attr {
	switch value := value.(type) {
	case string:
		return slog.String(key, value)
	case bool:
		return slog.Bool(key, value)
	case json.Number:
		if n, err := value.Int64(); err == nil {
			return slog.Int64(key, n)
		}
		if n, err := strconv.ParseUint(value.String(), 10, 64); err == nil {
			return slog.Uint64(key, n)
		}
		if n, err := value.Float64(); err == nil {
			return slog.Float64(key, n)
		}
	}
	return slog.Any(key, value)
}
