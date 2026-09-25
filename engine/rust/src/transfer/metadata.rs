//! Backup `Info.plist` construction and restore app-list staging.

use std::io::{Read, Write};
use std::path::Path;
use std::time::SystemTime;

use idevice::services::afc::{opcode::AfcFopenMode, AfcClient};
use idevice::services::installation_proxy::InstallationProxyClient;
use idevice::services::mobilebackup2::BackupDelegate;
use idevice::services::springboardservices::SpringBoardServicesClient;
use idevice::utils::plist::truncate_dates_to_seconds;
use idevice::{IdeviceError, IdeviceService};
use plist::Value;

use crate::afc::{read_small_file, FileGuard};
use crate::provider::{authed_lockdown, AirvaultProvider};
use crate::timeouts;

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

// Info.plist identity keys and the lockdown root-domain values they copy
// (idevicebackup2's mobilebackup_factory_info_plist_new).
const IDENTITY_KEYS: [(&str, &str); 10] = [
    ("Build Version", "BuildVersion"),
    ("Device Name", "DeviceName"),
    ("Display Name", "DeviceName"),
    ("ICCID", "IntegratedCircuitCardIdentity"),
    ("IMEI", "InternationalMobileEquipmentIdentity"),
    ("MEID", "MobileEquipmentIdentifier"),
    ("Phone Number", "PhoneNumber"),
    ("Product Type", "ProductType"),
    ("Product Version", "ProductVersion"),
    ("Serial Number", "SerialNumber"),
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
    let stage = async {
        let mut afc = AfcClient::connect(provider).await?;
        let _ = afc.mk_dir("/iTunesRestore").await;
        let path = "/iTunesRestore/RestoreApplications.plist";
        let mut file = FileGuard::new(afc.open_owned(path, AfcFopenMode::WrOnly).await?);
        file.write_entire(&xml).await?;
        file.close().await
    };
    stage
        .await
        .map_err(|error: IdeviceError| RestoreApplicationsError::Device(format!("{error:?}")))?;
    Ok(true)
}

/// A failed or aborted restore must not leave the staged app list on the device
/// (idevicebackup2 parity). Best-effort: it may already be rebooting.
pub(super) async fn remove_restore_applications(provider: &AirvaultProvider, udid: &str) {
    let cleanup = async {
        let mut afc = AfcClient::connect(provider).await?;
        afc.remove_all("/iTunesRestore").await
    };
    match tokio::time::timeout(timeouts::CONNECT, cleanup).await {
        Ok(Ok(())) => {}
        Ok(Err(error)) => {
            tracing::debug!(udid = %udid, ?error, "restore: staged app list not removed")
        }
        Err(_) => tracing::debug!(udid = %udid, "restore: staged app list removal timed out"),
    }
}

/// A file's non-empty contents, or None. Takes the client out of `afc` and puts
/// it back unless the read lost it.
async fn afc_file_contents(afc: &mut Option<AfcClient>, path: &str) -> Option<Vec<u8>> {
    match read_small_file(afc.take()?, path, usize::MAX).await {
        Ok((client, data)) => {
            *afc = Some(client);
            Some(data).filter(|data| !data.is_empty())
        }
        Err((client, _)) => {
            *afc = client;
            None
        }
    }
}

async fn build_info_plist(provider: &AirvaultProvider, udid: &str) -> Result<Vec<u8>, String> {
    let mut lockdown = authed_lockdown(provider)
        .await
        .map_err(|error| format!("{error:?}"))?;
    let root = lockdown
        .get_value(None, None)
        .await
        .ok()
        .and_then(Value::into_dictionary)
        .unwrap_or_default();
    let mut info = plist::Dictionary::new();
    for (info_key, lockdown_key) in IDENTITY_KEYS {
        if let Some(value) = root.get(lockdown_key).and_then(Value::as_string) {
            if !value.is_empty() {
                info.insert(info_key.into(), Value::String(value.into()));
            }
        }
    }

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
    let mut now = Value::Date(SystemTime::now().into());
    truncate_dates_to_seconds(&mut now);
    info.insert("Last Backup Date".into(), now);

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

    let mut afc = AfcClient::connect(provider)
        .await
        .inspect_err(|error| {
            tracing::warn!(
                ?error,
                "afc unavailable; Info.plist carries no iTunes files"
            )
        })
        .ok();
    if let Some(data) = afc_file_contents(&mut afc, "/Books/iBooksData2.plist").await {
        info.insert("iBooks Data 2".into(), Value::Data(data));
    }
    let mut itunes_files = plist::Dictionary::new();
    for name in ITUNES_FILES {
        let path = format!("/iTunes_Control/iTunes/{name}");
        if let Some(data) = afc_file_contents(&mut afc, &path).await {
            itunes_files.insert(name.into(), Value::Data(data));
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
        if let Some(client) = springboard.as_mut() {
            match client.get_icon_pngdata(bundle.to_owned()).await {
                Ok(png) if !png.is_empty() => {
                    application.insert("PlaceholderIcon".into(), Value::Data(png));
                }
                Ok(_) => {}
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
                    if !client_usable {
                        springboard = None;
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
