//! Device-level controls and reads: power actions, the hardware/storage/battery
//! snapshot and the rendered wallpaper previews.

use std::ffi::c_char;

use idevice::services::diagnostics_relay::DiagnosticsRelayClient;
use idevice::services::springboardservices::SpringBoardServicesClient;
use idevice::{IdeviceError, IdeviceService};
use plist::Value;
use tracing::Instrument;

use crate::engine_error::EngineFailure;
use crate::ffi::{
    block_bounded, engine_udid, guard_error, opt_owned, out_buffer, out_str, AvBuffer, AvEngine,
    AvError,
};
use crate::logging::operation_span;
use crate::provider::{authed_lockdown, provider_for, EngineContext};
use crate::timeouts;

/// Sends a power command (0 = restart, 1 = shutdown, 2 = sleep) via the
/// diagnostics relay. Returns once the command is ACCEPTED — the device acts
/// asynchronously and (for restart/shutdown) drops off the muxer.
#[no_mangle]
pub extern "C" fn av_device_power(
    engine: *mut AvEngine,
    udid: *const c_char,
    job_id: *const c_char,
    action: i32,
    error: *mut AvError,
) -> i32 {
    guard_error(error, || {
        let (engine, udid) = unsafe { engine_udid(engine, udid) }?;
        let context = engine.context();
        let job_id = unsafe { opt_owned(job_id) };
        if !(0..=2).contains(&action) {
            return Err(EngineFailure::invalid_argument("bad power action"));
        }
        let span = operation_span(&job_id, "power", &udid);
        block_bounded(
            timeouts::UI_CALL,
            "power request timed out",
            async move {
                let provider = provider_for(context, &udid).await?;
                let mut dr = DiagnosticsRelayClient::connect(&provider).await?;
                match action {
                    0 => dr.restart().await,
                    1 => dr.shutdown().await,
                    _ => dr.sleep().await,
                }
            }
            .instrument(span),
        )
    })
}

/// Reads hardware/storage identity in one pass: lockdown values, the
/// com.apple.disk_usage domain (session-gated) and gas-gauge battery details.
/// Every field is best-effort — an unexposed key stays zero/empty (omitted).
async fn device_info_inner(context: &EngineContext, udid: &str) -> Result<String, IdeviceError> {
    let provider = provider_for(context, udid).await?;
    let mut lc = authed_lockdown(&provider).await?;

    // Thin transport: dump raw domains, let Go select and interpret fields. The
    // whole lockdown root arrives in one round-trip, so a dozing phone cannot
    // blow the device-info budget on a dozen serial per-key queries.
    let mut out = serde_json::Map::new();
    if let Ok(root) = lc.get_value(None, None).await {
        out.insert(
            "lockdown".into(),
            serde_json::to_value(&root).unwrap_or_default(),
        );
    }
    if let Ok(disk) = lc.get_value(None, Some("com.apple.disk_usage")).await {
        out.insert(
            "diskUsage".into(),
            serde_json::to_value(&disk).unwrap_or_default(),
        );
    }
    // Find My gates every restore (MBErrorDomain/211); expose the flag so the
    // UI can block one before it starts. Only an affirmative boolean is
    // forwarded — anything else must read as unknown, never break the dump.
    if let Ok(fmip) = lc
        .get_value(Some("IsAssociated"), Some("com.apple.fmip"))
        .await
    {
        if let Some(enabled) = fmip.as_boolean() {
            out.insert("findMy".into(), enabled.into());
        }
    }
    // Battery gas-gauge via the diagnostics relay (AppleSmartBattery IORegistry):
    // USB-mostly and can HANG over a Wi-Fi/standby link, so hard-capped and
    // best-effort — on timeout/absence the key is simply omitted.
    let battery = tokio::time::timeout(timeouts::PROBE, async {
        let mut dr = DiagnosticsRelayClient::connect(&provider).await.ok()?;
        dr.ioregistry(None, Some("AppleSmartBattery"), None)
            .await
            .ok()
            .flatten()
    })
    .await
    .ok()
    .flatten();
    if let Some(b) = battery {
        out.insert(
            "battery".into(),
            serde_json::to_value(Value::Dictionary(b)).unwrap_or_default(),
        );
    }

    Ok(serde_json::Value::Object(out).to_string())
}

/// Hardware/storage/battery-health snapshot as JSON (see device_info_inner).
#[no_mangle]
pub extern "C" fn av_device_hardware(
    engine: *mut AvEngine,
    udid: *const c_char,
    out_json: *mut *mut c_char,
    error: *mut AvError,
) -> i32 {
    guard_error(error, || {
        let (engine, udid) = unsafe { engine_udid(engine, udid) }?;
        let json = block_bounded(
            timeouts::UI_CALL,
            "device info timed out",
            device_info_inner(engine.context(), &udid),
        )?;
        out_str(out_json, &json);
        Ok(())
    })
}

/// The rendered lock- or home-screen wallpaper preview.
async fn wallpaper_inner(
    context: &EngineContext,
    udid: &str,
    lock_screen: bool,
) -> Result<Vec<u8>, IdeviceError> {
    let provider = provider_for(context, udid).await?;
    let mut sb = SpringBoardServicesClient::connect(&provider).await?;
    if lock_screen {
        sb.get_lock_screen_wallpaper_preview_pngdata().await
    } else {
        sb.get_home_screen_wallpaper_preview_pngdata().await
    }
}

/// Fetches a rendered wallpaper preview as raw PNG. `lock_screen` selects
/// lock (non-zero) or home (zero).
#[no_mangle]
pub extern "C" fn av_wallpaper_get(
    engine: *mut AvEngine,
    udid: *const c_char,
    lock_screen: i32,
    out: *mut AvBuffer,
    error: *mut AvError,
) -> i32 {
    guard_error(error, || {
        let (engine, udid) = unsafe { engine_udid(engine, udid) }?;
        let png = block_bounded(
            timeouts::UI_CALL,
            "wallpaper preview timed out",
            wallpaper_inner(engine.context(), &udid, lock_screen != 0),
        )?;
        if !out_buffer(out, png) {
            return Err(EngineFailure::invalid_argument(
                "missing wallpaper output buffer",
            ));
        }
        Ok(())
    })
}

#[cfg(test)]
mod tests {
    use plist::{Dictionary, Value};

    // Locks the device_info wire contract: serde_json must serialize the raw
    // plist domains to the plain JSON shape internal/engine/hardware.go parses
    // (strings, signed/unsigned numbers, nested arrays of dicts).
    #[test]
    fn serde_json_serializes_lockdown_shape() {
        let mut carrier = Dictionary::new();
        carrier.insert(
            "CFBundleIdentifier".into(),
            Value::String("com.apple.MTS_ru".into()),
        );
        carrier.insert("Slot".into(), Value::String("kOne".into()));
        let mut root = Dictionary::new();
        root.insert("SerialNumber".into(), Value::String("ABC123".into()));
        root.insert("InstantAmperage".into(), Value::Integer((-420i64).into()));
        root.insert("MaxCapacity".into(), Value::Integer(5000u64.into()));
        root.insert(
            "CarrierBundleInfoArray".into(),
            Value::Array(vec![Value::Dictionary(carrier)]),
        );

        let json = serde_json::to_value(Value::Dictionary(root)).unwrap();
        assert_eq!(json["SerialNumber"], "ABC123");
        assert_eq!(json["InstantAmperage"], -420);
        assert_eq!(json["MaxCapacity"], 5000);
        assert_eq!(
            json["CarrierBundleInfoArray"][0]["CFBundleIdentifier"],
            "com.apple.MTS_ru"
        );
        assert_eq!(json["CarrierBundleInfoArray"][0]["Slot"], "kOne");
    }
}
