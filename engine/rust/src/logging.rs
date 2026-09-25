//! The tracing → Go log bridge: every shim and idevice event reaches the host
//! logger as (level, target, message, flat JSON fields).

use std::collections::BTreeMap;
use std::ffi::c_char;
use std::sync::OnceLock;

use serde_json::Value as JsonValue;
use tracing::field::{Field, Visit};
use tracing::{Event, Subscriber};
use tracing_subscriber::layer::{Context as LayerContext, SubscriberExt};
use tracing_subscriber::registry::LookupSpan;
use tracing_subscriber::util::SubscriberInitExt;
use tracing_subscriber::Layer;

use crate::ffi::{ffi_cstring, guard, in_str};

/// Structured tracing event callback into the Go host. Strings are borrowed
/// for the duration of the call; `fields_json` is a flat JSON object.
type LogCb = extern "C" fn(i32, *const c_char, *const c_char, *const c_char);

static LOG_CALLBACK: OnceLock<LogCb> = OnceLock::new();
static LOG_FILTER: OnceLock<String> = OnceLock::new();
static TRACING_READY: OnceLock<bool> = OnceLock::new();

#[derive(Default)]
struct LogFields(BTreeMap<String, JsonValue>);

impl Visit for LogFields {
    fn record_i64(&mut self, field: &Field, value: i64) {
        self.0.insert(field.name().to_owned(), value.into());
    }

    fn record_u64(&mut self, field: &Field, value: u64) {
        self.0.insert(field.name().to_owned(), value.into());
    }

    fn record_bool(&mut self, field: &Field, value: bool) {
        self.0.insert(field.name().to_owned(), value.into());
    }

    fn record_f64(&mut self, field: &Field, value: f64) {
        self.0.insert(field.name().to_owned(), value.into());
    }

    fn record_str(&mut self, field: &Field, value: &str) {
        self.0
            .insert(field.name().to_owned(), JsonValue::String(value.to_owned()));
    }

    fn record_debug(&mut self, field: &Field, value: &dyn std::fmt::Debug) {
        self.0.insert(
            field.name().to_owned(),
            JsonValue::String(format!("{value:?}")),
        );
    }
}

struct GoLogLayer;

impl<S> Layer<S> for GoLogLayer
where
    S: Subscriber + for<'lookup> LookupSpan<'lookup>,
{
    fn on_new_span(
        &self,
        attrs: &tracing::span::Attributes<'_>,
        id: &tracing::span::Id,
        ctx: LayerContext<'_, S>,
    ) {
        let mut fields = LogFields::default();
        attrs.record(&mut fields);
        if let Some(span) = ctx.span(id) {
            span.extensions_mut().insert(fields);
        }
    }

    fn on_record(
        &self,
        id: &tracing::span::Id,
        values: &tracing::span::Record<'_>,
        ctx: LayerContext<'_, S>,
    ) {
        if let Some(span) = ctx.span(id) {
            let mut extensions = span.extensions_mut();
            if let Some(fields) = extensions.get_mut::<LogFields>() {
                values.record(fields);
            }
        }
    }

    fn on_event(&self, event: &Event<'_>, ctx: LayerContext<'_, S>) {
        let Some(callback) = LOG_CALLBACK.get().copied() else {
            return;
        };
        let mut fields = LogFields::default();
        if let Some(scope) = ctx.event_scope(event) {
            for span in scope.from_root() {
                if let Some(span_fields) = span.extensions().get::<LogFields>() {
                    fields.0.extend(span_fields.0.clone());
                }
            }
        }
        event.record(&mut fields);

        let message = fields
            .0
            .remove("message")
            .and_then(|value| value.as_str().map(str::to_owned))
            .unwrap_or_else(|| event.metadata().name().to_owned());
        let fields_json = serde_json::to_string(&fields.0).unwrap_or_else(|_| "{}".to_owned());
        let target = ffi_cstring(event.metadata().target());
        let message = ffi_cstring(&message);
        let fields_json = ffi_cstring(&fields_json);
        callback(
            tracing_level(event.metadata().level()),
            target.as_ptr(),
            message.as_ptr(),
            fields_json.as_ptr(),
        );
    }
}

fn tracing_level(level: &tracing::Level) -> i32 {
    match *level {
        tracing::Level::ERROR => 8,
        tracing::Level::WARN => 4,
        tracing::Level::INFO => 0,
        tracing::Level::DEBUG => -4,
        tracing::Level::TRACE => -8,
    }
}

// RUST_LOG wins: dependency logs are only useful one module at a time, since
// idevice dumps AFC packets and whole plists at debug.
fn tracing_filter() -> tracing_subscriber::EnvFilter {
    if let Ok(filter) = tracing_subscriber::EnvFilter::try_from_default_env() {
        return filter;
    }
    match LOG_FILTER.get() {
        Some(filter) => tracing_subscriber::EnvFilter::new(filter),
        None => tracing_subscriber::EnvFilter::new("warn"),
    }
}

pub(crate) fn init_tracing() -> bool {
    *TRACING_READY.get_or_init(|| {
        if LOG_CALLBACK.get().is_some() {
            tracing_subscriber::registry()
                .with(tracing_filter())
                .with(GoLogLayer)
                .try_init()
                .is_ok()
        } else {
            tracing_subscriber::fmt()
                .with_env_filter(tracing_filter())
                .with_ansi(false)
                .with_writer(std::io::stderr)
                .try_init()
                .is_ok()
        }
    })
}

/// Connects Rust tracing to the host logger before the runtime starts. The
/// selected application level applies to this crate; dependency warnings stay
/// available without enabling their high-volume info/debug streams.
///
/// # Safety
/// `level` must be null or point to a valid NUL-terminated string for the call.
#[no_mangle]
pub unsafe extern "C" fn av_log_init(cb: LogCb, level: *const c_char) -> i32 {
    guard(std::ptr::null_mut(), || {
        let level = unsafe { in_str(level) }
            .filter(|s| !s.is_empty())
            .unwrap_or("info")
            .to_ascii_lowercase();
        let filter = format!("warn,airvault_shim={level}");
        let _ = LOG_CALLBACK.set(cb);
        let _ = LOG_FILTER.set(filter);
        if init_tracing() {
            0
        } else {
            1
        }
    })
}

/// Span shared by every durable mutation that crosses the Go/Rust boundary.
pub(crate) fn operation_span(job_id: &str, operation: &str, udid: &str) -> tracing::Span {
    let span = tracing::info_span!(
        "operation",
        job_id = tracing::field::Empty,
        operation = %operation,
        udid = %udid
    );
    if !job_id.is_empty() {
        span.record("job_id", job_id);
    }
    span
}

#[cfg(test)]
mod tests {
    use std::ffi::CStr;
    use std::sync::Mutex;

    use super::*;

    type CapturedLog = (i32, String, String, String);
    static CAPTURED_LOG: Mutex<Option<CapturedLog>> = Mutex::new(None);

    extern "C" fn capture_log(
        level: i32,
        target: *const c_char,
        message: *const c_char,
        fields: *const c_char,
    ) {
        let copy = |value| unsafe { CStr::from_ptr(value).to_string_lossy().into_owned() };
        *CAPTURED_LOG.lock().unwrap() = Some((level, copy(target), copy(message), copy(fields)));
    }

    #[test]
    fn go_log_layer_forwards_span_and_event_fields() {
        assert!(LOG_CALLBACK.set(capture_log).is_ok());
        let subscriber = tracing_subscriber::registry().with(GoLogLayer);
        tracing::subscriber::with_default(subscriber, || {
            let span = tracing::info_span!("operation", job_id = "job-123", udid = "device-1");
            span.in_scope(|| tracing::warn!(error = "retryable", "retry scheduled"));
        });

        let captured = CAPTURED_LOG.lock().unwrap().take().unwrap();
        assert_eq!(captured.0, 4);
        assert_eq!(captured.2, "retry scheduled");
        let fields: BTreeMap<String, JsonValue> = serde_json::from_str(&captured.3).unwrap();
        assert_eq!(fields["job_id"], "job-123");
        assert_eq!(fields["udid"], "device-1");
        assert_eq!(fields["error"], "retryable");
    }
}
