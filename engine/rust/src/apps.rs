//! Installed applications: the user-app list, .ipa install/uninstall and
//! home-screen icons.

use std::collections::HashMap;
use std::ffi::c_char;
use std::time::Duration;

use base64::prelude::{Engine as _, BASE64_STANDARD};
use idevice::services::afc::{errors::AfcError, opcode::AfcFopenMode, AfcClient};
use idevice::services::installation_proxy::InstallationProxyClient;
use idevice::services::springboardservices::SpringBoardServicesClient;
use idevice::{IdeviceError, IdeviceService};
use tokio_util::io::InspectWriter;
use tracing::Instrument;

use crate::afc::FileGuard;
use crate::engine_error::{EngineFailure, ErrorKind};
use crate::ffi::{
    block_bounded, engine_udid, guard_error, opt_owned, out_str, req_str, to_json, AvEngine,
    AvError,
};
use crate::logging::operation_span;
use crate::provider::{provider_for, EngineContext};
use crate::timeouts;

/// Installing streams the whole .ipa and then waits on installd.
const APP_INSTALL_TIMEOUT: Duration = Duration::from_secs(300);
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
        if !matches!(error, IdeviceError::Afc(AfcError::ObjectNotFound)) {
            tracing::warn!(%error, "staged IPA cleanup failed");
        }
    }
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

/// User-installed applications as JSON [{"bundleId","name","version","fileSharing"}],
/// sorted by display name. Per-app disk size is deliberately absent — modern
/// iOS reports it as 0 via installation_proxy.
async fn apps_inner(context: &EngineContext, udid: &str) -> Result<String, IdeviceError> {
    let provider = provider_for(context, udid).await?;
    let mut ip = InstallationProxyClient::connect(&provider).await?;
    let apps = ip.get_apps(Some("User"), None).await?;

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

/// Lists user-installed applications into out_json (see apps_inner). The
/// generous timeout covers devices with hundreds of apps — the lookup answer is big.
#[no_mangle]
pub extern "C" fn av_apps_list(
    engine: *mut AvEngine,
    udid: *const c_char,
    out_json: *mut *mut c_char,
    error: *mut AvError,
) -> i32 {
    guard_error(error, || {
        let (engine, udid) = unsafe { engine_udid(engine, udid) }?;
        let json = block_bounded(
            timeouts::DEVICE_WORK,
            "app list timed out",
            apps_inner(engine.context(), &udid),
        )?;
        out_str(out_json, &json);
        Ok(())
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
) -> Result<(), EngineFailure> {
    // Standard installation_proxy + AFC path (ideviceinstaller-style), no sync
    // session; netmuxd keeps Wi-Fi device liveness independent of this call.
    cb(callback_id, AV_INSTALL_PHASE_STAGING, 0);
    let provider = provider_for(context, udid)
        .await
        .map_err(|e| EngineFailure::from_request("device provider lookup failed", e))?;

    // 1. Stream the package into the AFC jail (installs read from PublicStaging).
    // 1 MiB chunks: AFC does one device round-trip per write, so small buffers
    // collapse throughput over Wi-Fi.
    let file = tokio::fs::File::open(ipa_path)
        .await
        .map_err(|e| EngineFailure::new(ErrorKind::Internal, format!("read ipa: {e}")))?;
    let total = file
        .metadata()
        .await
        .map_err(|e| EngineFailure::new(ErrorKind::Internal, format!("read ipa size: {e}")))?
        .len();
    let mut afc = AfcClient::connect(&provider)
        .await
        .map_err(|e| EngineFailure::from_request("connect AFC", e))?;
    let _ = afc.mk_dir("PublicStaging").await; // best-effort — usually present
    let mut staged = FileGuard::new(
        afc.open_owned(INSTALL_STAGING_PATH, AfcFopenMode::WrOnly)
            .await
            .map_err(|e| EngineFailure::from_request("open staged IPA", e))?,
    );
    let mut copied = 0_u64;
    let mut last_percent = 0;
    let mut writer = InspectWriter::new(&mut *staged, |bytes| {
        copied += bytes.len() as u64;
        let percent = copied.min(total).saturating_mul(100) / total.max(1);
        if percent != last_percent {
            last_percent = percent;
            cb(callback_id, AV_INSTALL_PHASE_STAGING, percent);
        }
    });
    let mut reader = tokio::io::BufReader::with_capacity(1024 * 1024, file);
    let upload = tokio::io::copy_buf(&mut reader, &mut writer)
        .await
        .map_err(|e| EngineFailure::new(ErrorKind::Internal, format!("upload IPA: {e}")));
    let afc = staged
        .close()
        .await
        .map_err(|e| EngineFailure::from_request("close staged IPA", e));
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
        let mut ip = InstallationProxyClient::connect(&provider)
            .await
            .map_err(|e| EngineFailure::from_request("connect installation proxy", e))?;
        ip.install_with_callback(
            INSTALL_STAGING_PATH,
            None,
            move |(percent, ()): (u64, ())| async move {
                cb(callback_id, AV_INSTALL_PHASE_INSTALLING, percent);
            },
            (),
        )
        .await
        .map_err(|e| EngineFailure::from_request("install IPA", e))?;
        cb(callback_id, AV_INSTALL_PHASE_INSTALLING, 100);
        Ok(())
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
    guard_error(error, || {
        let (engine, udid) = unsafe { engine_udid(engine, udid) }?;
        let job_id = unsafe { opt_owned(job_id) };
        let path = unsafe { req_str(ipa_path, "bad ipa path") }?;
        let span = operation_span(&job_id, "install", &udid);
        block_bounded(
            APP_INSTALL_TIMEOUT,
            "install timed out",
            app_install(engine.context(), &udid, &path, cb, callback_id).instrument(span),
        )
    })
}

/// Most icons one av_app_icons call reads.
const MAX_ICON_BATCH: usize = 100;

/// Home-screen icon PNGs by bundle id, read over one springboard connection.
/// A failed or empty icon is left out; a broken connection ends the batch
/// with what it already has.
pub(crate) async fn read_app_icons(
    springboard: &mut SpringBoardServicesClient,
    bundle_ids: &[String],
) -> HashMap<String, Vec<u8>> {
    let mut icons = HashMap::new();
    for bundle_id in bundle_ids {
        match springboard.get_icon_pngdata(bundle_id.clone()).await {
            Ok(png) if !png.is_empty() => {
                icons.insert(bundle_id.clone(), png);
            }
            Ok(_) => {}
            Err(error) => {
                // These fail one icon; any other error means the connection is gone.
                let connection_usable = matches!(
                    &error,
                    IdeviceError::UnexpectedResponse(_)
                        | IdeviceError::NotFound
                        | IdeviceError::GetProhibited
                );
                tracing::debug!(%bundle_id, ?error, connection_usable, "app icon read failed");
                if !connection_usable {
                    break;
                }
            }
        }
    }
    icons
}

async fn app_icons_inner(
    context: &EngineContext,
    udid: &str,
    bundle_ids: &[String],
) -> Result<HashMap<String, Vec<u8>>, IdeviceError> {
    let provider = provider_for(context, udid).await?;
    let mut springboard = SpringBoardServicesClient::connect(&provider).await?;
    Ok(read_app_icons(&mut springboard, bundle_ids).await)
}

/// Reads the icons for a JSON array of bundle ids into out_json as
/// {"<bundleId>": "<base64 PNG>"}; apps without a readable icon are absent.
#[no_mangle]
pub extern "C" fn av_app_icons(
    engine: *mut AvEngine,
    udid: *const c_char,
    bundle_ids_json: *const c_char,
    out_json: *mut *mut c_char,
    error: *mut AvError,
) -> i32 {
    guard_error(error, || {
        let (engine, udid) = unsafe { engine_udid(engine, udid) }?;
        let json = unsafe { req_str(bundle_ids_json, "bad bundle id list") }?;
        let bundle_ids: Vec<String> = serde_json::from_str(&json)
            .map_err(|e| EngineFailure::invalid_argument(format!("bad bundle id list: {e}")))?;
        if bundle_ids.len() > MAX_ICON_BATCH {
            return Err(EngineFailure::invalid_argument("too many bundle ids"));
        }
        let icons = block_bounded(
            timeouts::DEVICE_WORK,
            "app icons timed out",
            app_icons_inner(engine.context(), &udid, &bundle_ids),
        )?;
        let encoded: HashMap<String, String> = icons
            .into_iter()
            .map(|(bundle_id, png)| (bundle_id, BASE64_STANDARD.encode(png)))
            .collect();
        out_str(out_json, &to_json(&encoded));
        Ok(())
    })
}

async fn app_uninstall_inner(
    context: &EngineContext,
    udid: &str,
    bundle_id: &str,
) -> Result<(), IdeviceError> {
    let provider = provider_for(context, udid).await?;
    let mut ip = InstallationProxyClient::connect(&provider).await?;
    ip.uninstall(bundle_id.to_owned(), None).await
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
    guard_error(error, || {
        let (engine, udid) = unsafe { engine_udid(engine, udid) }?;
        let job_id = unsafe { opt_owned(job_id) };
        let bundle = unsafe { req_str(bundle_id, "bad bundle id") }?;
        let span = operation_span(&job_id, "uninstall", &udid);
        block_bounded(
            timeouts::DEVICE_WORK,
            "uninstall timed out",
            app_uninstall_inner(engine.context(), &udid, &bundle).instrument(span),
        )
    })
}
