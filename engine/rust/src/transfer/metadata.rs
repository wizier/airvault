//! Backup `Info.plist` construction and restore app-list staging.

use std::io::{Read, Write};
use std::path::Path;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use idevice::services::afc::{opcode::AfcFopenMode, AfcClient};
use idevice::services::installation_proxy::InstallationProxyClient;
use idevice::services::mobilebackup2::BackupDelegate;
use idevice::services::springboardservices::SpringBoardServicesClient;
use idevice::{IdeviceError, IdeviceService};
use plist::Value;

use crate::{getv_str, AirvaultProvider};

pub(super) const RESTORE_APPLICATIONS_TIMEOUT: Duration = Duration::from_secs(20);
// The canonical "iTunes Files" census (idevicebackup2).
const ITUNES_FILES: [&str; 11] = [
    "ApertureAlbumPrefs",
    "IC-Info.sidb",
    "IC-Info.sidv",
    "PhotosFolderAlbums",
    "PhotosFolderName",
    "PhotosFolderPrefs",
    "VoiceMemos.plist",
    "iPhotoAlbumPrefs",
    "iTunesApplicationIDs",
    "iTunesPrefs",
    "iTunesPrefs.plist",
];

pub(super) enum RestoreApplicationsError {
    Snapshot(String),
    Device(String),
}

pub(super) async fn prepare_backup_info(
    provider: &AirvaultProvider,
    udid: &str,
    storage: &dyn BackupDelegate,
    info_path: &Path,
) -> Result<(), String> {
    let bytes = build_info_plist(provider, udid).await?;
    let mut file = storage
        .create_file_write(info_path)
        .await
        .map_err(|error| error.to_string())?;
    file.write_all(&bytes).map_err(|error| error.to_string())?;
    file.flush().map_err(|error| error.to_string())
}

/// Copy the backup's application census to the phone so iOS can reinstall
/// App Store applications after restore.
pub(super) async fn stage_restore_applications(
    provider: &AirvaultProvider,
    storage: &dyn BackupDelegate,
    info_path: &Path,
) -> Result<bool, RestoreApplicationsError> {
    const MAX_INFO_BYTES: u64 = 256 << 20;

    let mut file = storage
        .open_file_read(info_path)
        .await
        .map_err(|error| RestoreApplicationsError::Snapshot(error.to_string()))?;
    let mut bytes = Vec::new();
    file.by_ref()
        .take(MAX_INFO_BYTES + 1)
        .read_to_end(&mut bytes)
        .map_err(|error| RestoreApplicationsError::Snapshot(error.to_string()))?;
    if bytes.len() as u64 > MAX_INFO_BYTES {
        return Err(RestoreApplicationsError::Snapshot(
            "Info.plist exceeds 256 MiB".into(),
        ));
    }

    let info: Value = plist::from_bytes(&bytes)
        .map_err(|error| RestoreApplicationsError::Snapshot(error.to_string()))?;
    let Some(apps) = info
        .as_dictionary()
        .and_then(|dictionary| dictionary.get("Applications"))
    else {
        return Ok(false);
    };
    if apps.as_dictionary().is_none_or(|apps| apps.is_empty()) {
        return Ok(false);
    }

    let mut xml = Vec::new();
    apps.to_writer_xml(&mut xml)
        .map_err(|error| RestoreApplicationsError::Snapshot(error.to_string()))?;
    let mut afc = AfcClient::connect(provider)
        .await
        .map_err(|error| RestoreApplicationsError::Device(format!("{error:?}")))?;
    let _ = afc.mk_dir("/iTunesRestore").await;
    let mut file = afc
        .open(
            "/iTunesRestore/RestoreApplications.plist",
            AfcFopenMode::WrOnly,
        )
        .await
        .map_err(|error| RestoreApplicationsError::Device(format!("{error:?}")))?;
    file.write_entire(&xml)
        .await
        .map_err(|error| RestoreApplicationsError::Device(format!("{error:?}")))?;
    file.close()
        .await
        .map_err(|error| RestoreApplicationsError::Device(format!("{error:?}")))?;
    Ok(true)
}

/// A failed or aborted restore must not leave the staged app list on the device
/// (idevicebackup2 parity). Best-effort: it may already be rebooting.
pub(super) async fn remove_restore_applications(provider: &AirvaultProvider, udid: &str) {
    let cleanup = async {
        let mut afc = AfcClient::connect(provider).await?;
        afc.remove_all("/iTunesRestore").await
    };
    match tokio::time::timeout(RESTORE_APPLICATIONS_TIMEOUT, cleanup).await {
        Ok(Ok(())) => {}
        Ok(Err(error)) => {
            tracing::debug!(udid = %udid, ?error, "restore: staged app list not removed")
        }
        Err(_) => tracing::debug!(udid = %udid, "restore: staged app list removal timed out"),
    }
}

async fn afc_file_contents(afc: &mut AfcClient, path: &str) -> Option<Vec<u8>> {
    let mut file = afc.open(path, AfcFopenMode::RdOnly).await.ok()?;
    let data = file.read_entire().await;
    let _ = file.close().await;
    data.ok().filter(|data| !data.is_empty())
}

async fn build_info_plist(provider: &AirvaultProvider, udid: &str) -> Result<Vec<u8>, String> {
    let mut lockdown = crate::authed_lockdown(provider)
        .await
        .map_err(|error| format!("{error:?}"))?;
    let mut info = plist::Dictionary::new();
    let mut set = |key: &str, value: String| {
        if !value.is_empty() {
            info.insert(key.into(), Value::String(value));
        }
    };

    let name = getv_str(&mut lockdown, "DeviceName").await;
    set(
        "Build Version",
        getv_str(&mut lockdown, "BuildVersion").await,
    );
    set("Device Name", name.clone());
    set("Display Name", name);
    set(
        "ICCID",
        getv_str(&mut lockdown, "IntegratedCircuitCardIdentity").await,
    );
    set(
        "IMEI",
        getv_str(&mut lockdown, "InternationalMobileEquipmentIdentity").await,
    );
    set(
        "MEID",
        getv_str(&mut lockdown, "MobileEquipmentIdentifier").await,
    );
    set("Phone Number", getv_str(&mut lockdown, "PhoneNumber").await);
    set("Product Type", getv_str(&mut lockdown, "ProductType").await);
    set(
        "Product Version",
        getv_str(&mut lockdown, "ProductVersion").await,
    );
    set(
        "Serial Number",
        getv_str(&mut lockdown, "SerialNumber").await,
    );

    info.insert(
        "GUID".into(),
        Value::String(uuid::Uuid::new_v4().simple().to_string().to_uppercase()),
    );
    info.insert("Target Identifier".into(), Value::String(udid.into()));
    info.insert("Target Type".into(), Value::String("Device".into()));
    info.insert(
        "Unique Identifier".into(),
        Value::String(udid.to_uppercase()),
    );
    // CFDate only accepts whole seconds.
    let now = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|elapsed| UNIX_EPOCH + Duration::from_secs(elapsed.as_secs()))
        .unwrap_or(UNIX_EPOCH);
    info.insert("Last Backup Date".into(), Value::Date(now.into()));

    let itunes_version = lockdown
        .get_value(Some("MinITunesVersion"), Some("com.apple.mobile.iTunes"))
        .await
        .ok()
        .and_then(|value| value.as_string().map(str::to_owned))
        .unwrap_or_else(|| "10.0.1".into());
    info.insert("iTunes Version".into(), Value::String(itunes_version));
    let itunes_settings = lockdown
        .get_value(None, Some("com.apple.iTunes"))
        .await
        .unwrap_or(Value::Dictionary(plist::Dictionary::new()));
    info.insert("iTunes Settings".into(), itunes_settings);

    let mut itunes_files = plist::Dictionary::new();
    match AfcClient::connect(provider).await {
        Ok(mut afc) => {
            if let Some(data) = afc_file_contents(&mut afc, "/Books/iBooksData2.plist").await {
                info.insert("iBooks Data 2".into(), Value::Data(data));
            }
            for name in ITUNES_FILES {
                let path = format!("/iTunes_Control/iTunes/{name}");
                if let Some(data) = afc_file_contents(&mut afc, &path).await {
                    itunes_files.insert(name.into(), Value::Data(data));
                }
            }
        }
        Err(error) => {
            tracing::warn!(
                ?error,
                "afc unavailable; Info.plist carries no iTunes files"
            )
        }
    }
    info.insert("iTunes Files".into(), Value::Dictionary(itunes_files));

    let mut installation = InstallationProxyClient::connect(provider)
        .await
        .map_err(|error| format!("{error:?}"))?;
    let mut options = plist::Dictionary::new();
    options.insert("ApplicationType".into(), Value::String("User".into()));
    options.insert(
        "ReturnAttributes".into(),
        Value::Array(
            ["CFBundleIdentifier", "ApplicationSINF", "iTunesMetadata"]
                .into_iter()
                .map(|attribute| Value::String(attribute.into()))
                .collect(),
        ),
    );
    let apps = installation
        .browse(Some(Value::Dictionary(options)))
        .await
        .map_err(|error| format!("{error:?}"))?;
    let mut springboard = SpringBoardServicesClient::connect(provider)
        .await
        .map(Some)
        .unwrap_or_else(|error| {
            tracing::warn!(
                ?error,
                "sbservices unavailable; app census continues without placeholder icons"
            );
            None
        });

    let mut applications = plist::Dictionary::new();
    let mut installed = Vec::new();
    for app in &apps {
        let Some(dictionary) = app.as_dictionary() else {
            continue;
        };
        let Some(bundle) = dictionary
            .get("CFBundleIdentifier")
            .and_then(|value| value.as_string())
        else {
            continue;
        };
        installed.push(Value::String(bundle.into()));

        let (Some(sinf), Some(metadata)) = (
            dictionary.get("ApplicationSINF"),
            dictionary.get("iTunesMetadata"),
        ) else {
            continue;
        };
        let mut application = plist::Dictionary::new();
        application.insert("ApplicationSINF".into(), sinf.clone());
        if let Some(mut client) = springboard.take() {
            match client.get_icon_pngdata(bundle.to_owned()).await {
                Ok(png) => {
                    if !png.is_empty() {
                        application.insert("PlaceholderIcon".into(), Value::Data(png));
                    }
                    springboard = Some(client);
                }
                Err(error) => {
                    let client_usable = matches!(
                        &error,
                        IdeviceError::UnexpectedResponse(_)
                            | IdeviceError::NotFound
                            | IdeviceError::GetProhibited
                    );
                    tracing::warn!(
                        ?error,
                        client_usable,
                        "sbservices icon read failed; census continues without this icon"
                    );
                    if client_usable {
                        springboard = Some(client);
                    }
                }
            }
        }
        application.insert("iTunesMetadata".into(), metadata.clone());
        applications.insert(bundle.into(), Value::Dictionary(application));
    }

    tracing::debug!(
        apps = installed.len(),
        restorable = applications.len(),
        "backup: Info.plist app census"
    );
    info.insert("Applications".into(), Value::Dictionary(applications));
    info.insert("Installed Applications".into(), Value::Array(installed));

    let mut xml = Vec::new();
    Value::Dictionary(info)
        .to_writer_xml(&mut xml)
        .map_err(|error| error.to_string())?;
    Ok(xml)
}
