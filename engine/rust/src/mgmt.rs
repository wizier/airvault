//! Device management: power actions, the hardware/storage/battery snapshot,
//! installed applications, .ipa install/uninstall, app icons and wallpapers.

use std::ffi::c_char;
use std::time::Duration;

use idevice::services::afc::{errors::AfcError, opcode::AfcFopenMode, AfcClient};
use idevice::services::diagnostics_relay::DiagnosticsRelayClient;
use idevice::services::installation_proxy::InstallationProxyClient;
use idevice::services::springboardservices::SpringBoardServicesClient;
use idevice::IdeviceService;
use plist::Value;
use tokio_util::io::InspectWriter;
use tracing::Instrument;

use crate::engine_error::ErrorKind;
use crate::ffi::{engine_udid, guard_error, out_buffer, AvBuffer, AvEngine, AvError};
use crate::{
    block_bounded, block_bounded_out, block_bounded_unit, block_probe, operation_span, opt_owned,
    out_str, provider_for, req_str, to_json, AirvaultProvider, EngineContext, ProbeError,
};

const APP_INSTALL_OPERATION_TIMEOUT: Duration = Duration::from_secs(300);
const APP_UNINSTALL_TIMEOUT: Duration = Duration::from_secs(60);
const INSTALL_STAGING_PATH: &str = "PublicStaging/airvault-install.ipa";

/// The first non-empty candidate, or "" — the display-name/version fallback ladder.
fn first<'a>(candidates: &[&'a str]) -> &'a str {
    candidates
        .iter()
        .copied()
        .find(|s| !s.is_empty())
        .unwrap_or("")
}

/// Install progress into Go. Percent is local to the reported phase.
pub(crate) type InstallCb = extern "C" fn(usize, i32, u64);
pub const AV_INSTALL_PHASE_STAGING: i32 = 0;
pub const AV_INSTALL_PHASE_INSTALLING: i32 = 1;

async fn cleanup_staged_ipa(afc: &mut AfcClient) {
    if let Err(error) = afc.remove(INSTALL_STAGING_PATH).await {
        if !matches!(error, idevice::IdeviceError::Afc(AfcError::ObjectNotFound)) {
            tracing::warn!(%error, "staged IPA cleanup failed");
        }
    }
}

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
    guard_error(error, |err| {
        let Some((engine, udid)) = (unsafe { engine_udid(engine, udid, err) }) else {
            return ErrorKind::InvalidArgument.code();
        };
        let context = engine.context();
        let job_id = unsafe { opt_owned(job_id) };
        if !(0..=2).contains(&action) {
            out_str(err, "bad power action");
            return ErrorKind::InvalidArgument.code();
        }
        let span = operation_span(&job_id, "power", &udid);
        block_bounded_unit(
            err,
            Duration::from_secs(15),
            "power request timed out",
            async move {
                let fut = async {
                    let provider = provider_for(context, &udid).await?;
                    let mut dr = DiagnosticsRelayClient::connect(&provider).await?;
                    match action {
                        0 => dr.restart().await,
                        1 => dr.shutdown().await,
                        _ => dr.sleep().await,
                    }
                };
                fut.await.map_err(|e| format!("{e:?}"))
            }
            .instrument(span),
        )
    })
}

/// Reads hardware/storage identity in one pass: lockdown values, the
/// com.apple.disk_usage domain (session-gated) and gas-gauge battery details.
/// Every field is best-effort — an unexposed key stays zero/empty (omitted).
async fn device_info_inner(
    context: &EngineContext,
    udid: &str,
) -> Result<String, idevice::IdeviceError> {
    let provider = provider_for(context, udid).await?;
    let mut lc = crate::authed_lockdown(&provider).await?;

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
    let battery = tokio::time::timeout(Duration::from_secs(4), async {
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
    guard_error(error, |err| {
        let Some((engine, udid)) = (unsafe { engine_udid(engine, udid, err) }) else {
            return ErrorKind::InvalidArgument.code();
        };
        let context = engine.context();
        block_bounded_out(
            out_json,
            err,
            Duration::from_secs(15),
            "device info timed out",
            async move {
                device_info_inner(context, &udid)
                    .await
                    .map_err(|e| format!("{e:?}"))
            },
        )
    })
}

#[derive(serde::Serialize)]
#[serde(rename_all = "camelCase")]
struct AppItem {
    bundle_id: String,
    name: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    version: String,
    // UIFileSharingEnabled — the app exposes its Documents over house_arrest.
    file_sharing: bool,
}

/// Installed applications as JSON [{"bundleId","name","version","fileSharing"}],
/// sorted by display name. kind: 0 = user, 1 = system, 2 = any. Per-app disk size
/// is deliberately absent — modern iOS reports it as 0 via installation_proxy.
async fn apps_inner(
    context: &EngineContext,
    udid: &str,
    kind: i32,
) -> Result<String, idevice::IdeviceError> {
    let provider = provider_for(context, udid).await?;
    let mut ip = InstallationProxyClient::connect(&provider).await?;
    let app_type = match kind {
        0 => "User",
        1 => "System",
        _ => "Any",
    };
    let apps = ip.get_apps(Some(app_type), None).await?;

    let mut items: Vec<AppItem> = Vec::with_capacity(apps.len());
    for (bundle_id, info) in apps {
        let d = match info.as_dictionary() {
            Some(d) => d,
            None => continue,
        };
        let gets = |k: &str| d.get(k).and_then(|v| v.as_string()).unwrap_or("");
        let name = first(&[
            gets("CFBundleDisplayName"),
            gets("CFBundleName"),
            bundle_id.as_str(),
        ])
        .to_owned();
        let version =
            first(&[gets("CFBundleShortVersionString"), gets("CFBundleVersion")]).to_owned();
        let file_sharing = d
            .get("UIFileSharingEnabled")
            .and_then(|v| v.as_boolean())
            .unwrap_or(false);
        items.push(AppItem {
            bundle_id,
            name,
            version,
            file_sharing,
        });
    }
    items.sort_by_key(|a| a.name.to_lowercase());
    Ok(to_json(&items))
}

/// Lists installed applications into out_json (see apps_inner). The generous
/// timeout covers devices with hundreds of apps — the lookup answer is big.
#[no_mangle]
pub extern "C" fn av_apps_list(
    engine: *mut AvEngine,
    udid: *const c_char,
    kind: i32,
    out_json: *mut *mut c_char,
    error: *mut AvError,
) -> i32 {
    guard_error(error, |err| {
        let Some((engine, udid)) = (unsafe { engine_udid(engine, udid, err) }) else {
            return ErrorKind::InvalidArgument.code();
        };
        let context = engine.context();
        block_bounded_out(
            out_json,
            err,
            Duration::from_secs(30),
            "app list timed out",
            async move {
                apps_inner(context, &udid, kind)
                    .await
                    .map_err(|e| format!("{e:?}"))
            },
        )
    })
}

/// Streams an .ipa from `ipa_path` (a host file) into the device's AFC staging
/// area, then installs it. AirVault just delivers the package — iOS enforces
/// the Apple ID entitlement (FairPlay licence) at install/launch.
async fn app_install(
    context: &EngineContext,
    udid: &str,
    ipa_path: &str,
    cb: InstallCb,
    callback_id: usize,
) -> Result<(), String> {
    // Standard installation_proxy + AFC path (ideviceinstaller-style), no sync
    // session; netmuxd keeps Wi-Fi device liveness independent of this call.
    cb(callback_id, AV_INSTALL_PHASE_STAGING, 0);
    let provider = provider_for(context, udid)
        .await
        .map_err(|e| format!("device provider lookup failed: {e:?}"))?;
    app_install_run(&provider, ipa_path, cb, callback_id).await
}

async fn app_install_run(
    provider: &AirvaultProvider,
    ipa_path: &str,
    cb: InstallCb,
    callback_id: usize,
) -> Result<(), String> {
    // 1. Stream the package into the AFC jail (installs read from PublicStaging).
    // 1 MiB chunks: AFC does one device round-trip per write, so small buffers
    // collapse throughput over Wi-Fi.
    let file = tokio::fs::File::open(ipa_path)
        .await
        .map_err(|e| format!("read ipa: {e}"))?;
    let total = file
        .metadata()
        .await
        .map_err(|e| format!("read ipa size: {e}"))?
        .len();
    let mut afc = AfcClient::connect(provider)
        .await
        .map_err(|e| format!("connect AFC: {e:?}"))?;
    let _ = afc.mk_dir("PublicStaging").await; // best-effort — usually present
    let fd = afc
        .open_owned(INSTALL_STAGING_PATH, AfcFopenMode::WrOnly)
        .await
        .map_err(|e| format!("open staged IPA: {e:?}"))?;
    let mut copied = 0_u64;
    let mut last_percent = 0;
    let mut fd = InspectWriter::new(fd, |bytes| {
        copied += bytes.len() as u64;
        let percent = copied.min(total).saturating_mul(100) / total.max(1);
        if percent != last_percent {
            last_percent = percent;
            cb(callback_id, AV_INSTALL_PHASE_STAGING, percent);
        }
    });
    let mut reader = tokio::io::BufReader::with_capacity(1024 * 1024, file);
    let upload = tokio::io::copy_buf(&mut reader, &mut fd)
        .await
        .map_err(|e| format!("upload IPA: {e}"));
    let afc = fd
        .into_inner()
        .close()
        .await
        .map_err(|e| format!("close staged IPA: {e:?}"));
    if let Err(error) = upload {
        if let Ok(mut afc) = afc {
            cleanup_staged_ipa(&mut afc).await;
        }
        return Err(error);
    }
    let mut afc = afc?;
    cb(callback_id, AV_INSTALL_PHASE_STAGING, 100);

    // 2. Install from the staged path, forwarding installation_proxy's percent.
    let install = async {
        cb(callback_id, AV_INSTALL_PHASE_INSTALLING, 0);
        let mut ip = InstallationProxyClient::connect(provider)
            .await
            .map_err(|e| format!("connect installation proxy: {e:?}"))?;
        ip.install_with_callback(
            INSTALL_STAGING_PATH,
            None,
            move |(percent, ()): (u64, ())| async move {
                cb(callback_id, AV_INSTALL_PHASE_INSTALLING, percent);
            },
            (),
        )
        .await
        .map_err(|e| format!("install IPA: {e:?}"))?;
        cb(callback_id, AV_INSTALL_PHASE_INSTALLING, 100);
        Ok::<(), String>(())
    }
    .await;
    cleanup_staged_ipa(&mut afc).await;
    install
}

/// Installs a user-provided .ipa (host path) onto the device. Blocking, hard-
/// capped — install can take a while. Returns diagnostic text on failure.
#[no_mangle]
pub extern "C" fn av_app_install(
    engine: *mut AvEngine,
    udid: *const c_char,
    job_id: *const c_char,
    ipa_path: *const c_char,
    cb: InstallCb,
    callback_id: usize,
    error: *mut AvError,
) -> i32 {
    guard_error(error, |err| {
        let Some((engine, udid)) = (unsafe { engine_udid(engine, udid, err) }) else {
            return ErrorKind::InvalidArgument.code();
        };
        let context = engine.context();
        let job_id = unsafe { opt_owned(job_id) };
        let Some(path) = (unsafe { req_str(ipa_path, err, "bad ipa path") }) else {
            return ErrorKind::InvalidArgument.code();
        };
        let span = operation_span(&job_id, "install", &udid);
        block_bounded_unit(
            err,
            APP_INSTALL_OPERATION_TIMEOUT,
            "install timed out",
            app_install(context, &udid, &path, cb, callback_id).instrument(span),
        )
    })
}

/// One app's home-screen icon PNG.
async fn app_icon_inner(
    context: &EngineContext,
    udid: &str,
    bundle_id: &str,
) -> Result<Vec<u8>, idevice::IdeviceError> {
    let provider = provider_for(context, udid).await?;
    let mut sb = SpringBoardServicesClient::connect(&provider).await?;
    sb.get_icon_pngdata(bundle_id.to_owned()).await
}

/// Fetches one app icon into an owned binary buffer.
#[no_mangle]
pub extern "C" fn av_app_icon(
    engine: *mut AvEngine,
    udid: *const c_char,
    bundle_id: *const c_char,
    out: *mut AvBuffer,
    error: *mut AvError,
) -> i32 {
    guard_error(error, |err| {
        let Some((engine, udid)) = (unsafe { engine_udid(engine, udid, err) }) else {
            return ErrorKind::InvalidArgument.code();
        };
        let context = engine.context();
        let Some(bundle) = (unsafe { req_str(bundle_id, err, "bad bundle id") }) else {
            return ErrorKind::InvalidArgument.code();
        };
        block_bounded(
            err,
            Duration::from_secs(10),
            "app icon timed out",
            async move {
                app_icon_inner(context, &udid, &bundle)
                    .await
                    .map_err(|error| format!("{error:?}"))
            },
            move |bytes| {
                if out_buffer(out, bytes) {
                    0
                } else {
                    out_str(err, "missing app icon output buffer");
                    ErrorKind::InvalidArgument.code()
                }
            },
        )
    })
}

/// The rendered lock- or home-screen wallpaper preview.
async fn wallpaper_inner(
    context: &EngineContext,
    udid: &str,
    lock_screen: bool,
) -> Result<Vec<u8>, ProbeError> {
    let provider = provider_for(context, udid)
        .await
        .map_err(|e| ProbeError::Unreachable(format!("{e:?}")))?;
    let mut sb = SpringBoardServicesClient::connect(&provider)
        .await
        .map_err(|e| ProbeError::Unreachable(format!("{e:?}")))?;
    let png = if lock_screen {
        sb.get_lock_screen_wallpaper_preview_pngdata().await
    } else {
        sb.get_home_screen_wallpaper_preview_pngdata().await
    }
    .map_err(|e| ProbeError::Logical(format!("{e:?}")))?;
    Ok(png)
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
    guard_error(error, |err| {
        let Some((engine, udid)) = (unsafe { engine_udid(engine, udid, err) }) else {
            return ErrorKind::InvalidArgument.code();
        };
        let context = engine.context();
        block_probe(
            err,
            Duration::from_secs(10),
            "wallpaper preview timed out",
            wallpaper_inner(context, &udid, lock_screen != 0),
            move |bytes| {
                if out_buffer(out, bytes) {
                    0
                } else {
                    out_str(err, "missing wallpaper output buffer");
                    ErrorKind::InvalidArgument.code()
                }
            },
        )
    })
}

// ---- app uninstall ---------------------------------------------------------

async fn app_uninstall_inner(
    context: &EngineContext,
    udid: &str,
    bundle_id: &str,
) -> Result<(), String> {
    let provider = provider_for(context, udid)
        .await
        .map_err(|e| format!("{e:?}"))?;
    let mut ip = InstallationProxyClient::connect(&provider)
        .await
        .map_err(|e| format!("{e:?}"))?;
    ip.uninstall(bundle_id.to_owned(), None)
        .await
        .map_err(|e| format!("{e:?}"))
}

/// Uninstalls an app by bundle id (installation_proxy).
#[no_mangle]
pub extern "C" fn av_app_uninstall(
    engine: *mut AvEngine,
    udid: *const c_char,
    job_id: *const c_char,
    bundle_id: *const c_char,
    error: *mut AvError,
) -> i32 {
    guard_error(error, |err| {
        let Some((engine, udid)) = (unsafe { engine_udid(engine, udid, err) }) else {
            return ErrorKind::InvalidArgument.code();
        };
        let context = engine.context();
        let job_id = unsafe { opt_owned(job_id) };
        let Some(bundle) = (unsafe { req_str(bundle_id, err, "bad bundle id") }) else {
            return ErrorKind::InvalidArgument.code();
        };
        let span = operation_span(&job_id, "uninstall", &udid);
        block_bounded_unit(
            err,
            APP_UNINSTALL_TIMEOUT,
            "uninstall timed out",
            async move { app_uninstall_inner(context, &udid, &bundle).await }.instrument(span),
        )
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
