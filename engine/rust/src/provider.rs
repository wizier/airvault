//! The shared tokio runtime, the per-engine `EngineContext`, and how every
//! device conversation starts: the muxer list, a provider that authenticates
//! with AirVault's own pairing record, and the lockdown/service handshakes.

use std::future::Future;
use std::net::SocketAddr;
use std::path::{Path, PathBuf};
use std::pin::Pin;
use std::str::FromStr;
use std::sync::LazyLock;

use idevice::pairing_file::PairingFile;
use idevice::provider::{IdeviceProvider, UsbmuxdProvider};
use idevice::services::lockdown::LockdownClient;
use idevice::usbmuxd::{Connection, UsbmuxdAddr, UsbmuxdDevice};
use idevice::{Idevice, IdeviceError, IdeviceService};

use crate::bounded;
use crate::logging::init_tracing;
use crate::pairing_store::PairingStore;
use crate::timeouts;

// Shared multi-thread tokio runtime. Go always calls in from outside any
// runtime, so block_on is safe. av_log_init normally wires tracing to Go first;
// direct Rust callers retain a stderr fallback controlled by RUST_LOG.
static RT: LazyLock<tokio::runtime::Runtime> = LazyLock::new(|| {
    let _ = init_tracing();
    tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .expect("tokio runtime")
});

pub(crate) fn block<F: Future>(f: F) -> F::Output {
    RT.block_on(f)
}

/// Long-lived stream workers run as tasks on the shared runtime, not on
/// dedicated OS threads; close() joins them via block().
pub(crate) fn spawn<F>(f: F) -> tokio::task::JoinHandle<F::Output>
where
    F: Future + Send + 'static,
    F::Output: Send + 'static,
{
    RT.spawn(f)
}

/// Immutable provider and storage context shared by one explicit AvEngine.
/// Protocol code receives it as an ordinary dependency; process environment is
/// never consulted after construction.
#[derive(Debug)]
pub(crate) struct EngineContext {
    backup_root: PathBuf,
    pub(crate) pairing_store: PairingStore,
    mux_addr: UsbmuxdAddr,
}

impl EngineContext {
    pub(crate) fn new(
        backup_root: PathBuf,
        pairing_root: PathBuf,
        mux_address: Option<&str>,
    ) -> Result<Self, String> {
        let mux_addr = match mux_address.filter(|value| !value.is_empty()) {
            None => UsbmuxdAddr::default(),
            Some(value) => {
                #[cfg(unix)]
                {
                    if value.contains(':') {
                        UsbmuxdAddr::TcpSocket(
                            SocketAddr::from_str(value)
                                .map_err(|error| format!("invalid mux address: {error}"))?,
                        )
                    } else {
                        UsbmuxdAddr::UnixSocket(value.to_owned())
                    }
                }
                #[cfg(not(unix))]
                {
                    UsbmuxdAddr::TcpSocket(
                        SocketAddr::from_str(value)
                            .map_err(|error| format!("invalid mux address: {error}"))?,
                    )
                }
            }
        };
        Ok(Self {
            backup_root,
            pairing_store: PairingStore::new(pairing_root),
            mux_addr,
        })
    }

    pub(crate) fn backup_root(&self) -> &Path {
        &self.backup_root
    }

    pub(crate) fn mux_addr(&self) -> UsbmuxdAddr {
        self.mux_addr.clone()
    }
}

/// One crate-wide UDID length cap (matches the Go/store source limit).
pub(crate) const MAX_UDID_BYTES: usize = 64;

/// The muxer lists one entry per transport (USB + Wi-Fi = twice per udid);
/// collapse to one entry per device, preferring USB — faster, more reliable,
/// and some operations only make sense over the cable.
fn dedupe_prefer_usb(devices: Vec<UsbmuxdDevice>) -> Vec<UsbmuxdDevice> {
    let mut out: Vec<UsbmuxdDevice> = Vec::with_capacity(devices.len());
    for d in devices {
        match out.iter_mut().find(|e| e.udid == d.udid) {
            Some(e) => {
                if matches!(d.connection_type, Connection::Usb) {
                    *e = d;
                }
            }
            None => out.push(d),
        }
    }
    out
}

/// Devices currently on the muxer, one entry per device (USB preferred).
/// Time-bounded: every caller (discover/list/usb_list/provider_for and the
/// watch loop's snapshots) inherits the muxer cap from here.
pub(crate) async fn devices_deduped(
    context: &EngineContext,
) -> Result<Vec<UsbmuxdDevice>, IdeviceError> {
    bounded::within(timeouts::MUX, async {
        let mut mux = context.mux_addr().connect(0).await?;
        Ok(dedupe_prefer_usb(mux.get_devices().await?))
    })
    .await
}

/// Transports via usbmuxd but authenticates with AirVault's OWN pairing record;
/// get_pairing_file errors when we have none — which is how a device we never
/// paired reads as unpaired.
#[derive(Debug)]
pub(crate) struct AirvaultProvider {
    inner: UsbmuxdProvider,
    pairing: StoredPairing,
}

#[derive(Debug)]
enum StoredPairing {
    Missing,
    Present(Box<PairingFile>),
    Invalid(String),
}

impl IdeviceProvider for AirvaultProvider {
    fn connect(
        &self,
        port: u16,
    ) -> Pin<Box<dyn Future<Output = Result<Idevice, IdeviceError>> + Send>> {
        self.inner.connect(port)
    }
    fn label(&self) -> &str {
        self.inner.label()
    }
    fn get_pairing_file(
        &self,
    ) -> Pin<Box<dyn Future<Output = Result<PairingFile, IdeviceError>> + Send>> {
        let pairing = match &self.pairing {
            StoredPairing::Missing => Err(IdeviceError::InvalidHostID),
            StoredPairing::Present(pairing) => Ok(pairing.as_ref().clone()),
            StoredPairing::Invalid(detail) => Err(IdeviceError::UnexpectedResponse(format!(
                "AirVault pairing record is unavailable: {detail}"
            ))),
        };
        Box::pin(async move { pairing })
    }
}

/// A provider for one already-listed muxer entry, authenticating with
/// AirVault's own pairing record.
pub(crate) fn provider_from(context: &EngineContext, device: &UsbmuxdDevice) -> AirvaultProvider {
    let pairing = match context.pairing_store.load_pairing(&device.udid) {
        Ok(Some(pairing)) => StoredPairing::Present(Box::new(pairing)),
        Ok(None) => StoredPairing::Missing,
        Err(e) => StoredPairing::Invalid(e.to_string()),
    };
    AirvaultProvider {
        inner: device.to_provider(context.mux_addr(), "AirVault"),
        pairing,
    }
}

/// Picks the USB entry when the device is on both transports, so every
/// lockdown/mb2 conversation rides the cable whenever one is plugged in.
pub(crate) async fn provider_for(
    context: &EngineContext,
    udid: &str,
) -> Result<AirvaultProvider, IdeviceError> {
    let device = devices_deduped(context)
        .await?
        .into_iter()
        .find(|d| d.udid == udid)
        .ok_or(IdeviceError::DeviceNotFound)?;
    Ok(provider_from(context, &device))
}

/// Connect lockdown and open an authenticated session with our pairing record —
/// the preamble every session-gated domain read shares.
pub(crate) async fn authed_lockdown(
    provider: &AirvaultProvider,
) -> Result<LockdownClient, IdeviceError> {
    let mut lc = LockdownClient::connect(provider).await?;
    let pf = provider.get_pairing_file().await?;
    lc.start_session(&pf).await?;
    Ok(lc)
}

/// The authenticated start-service dance shared by single-connection daemons.
/// mobilebackup2 keeps its own variant (escrow bag + plain-session retry).
pub(crate) async fn connect_service(
    provider: &AirvaultProvider,
    service: &str,
) -> Result<Idevice, IdeviceError> {
    let pf = provider.get_pairing_file().await?;
    let mut lockdown = LockdownClient::connect(provider).await?;
    let legacy = lockdown.start_session(&pf).await?;
    let (port, ssl) = lockdown.start_service(service).await?;
    let mut connection = provider.connect(port).await?;
    if ssl {
        connection.start_session(&pf, legacy).await?;
    }
    Ok(connection)
}

/// Sends one u32-BE length-prefixed message — the framing plain services and
/// DeviceLink share; idevice keeps its own framed send/read private.
pub(crate) async fn send_framed(connection: &mut Idevice, body: &[u8]) -> Result<(), IdeviceError> {
    let mut framed = Vec::with_capacity(4 + body.len());
    framed.extend_from_slice(&(body.len() as u32).to_be_bytes());
    framed.extend_from_slice(body);
    connection.send_raw(&framed).await
}

/// Reads one length-prefixed message, refusing an empty or over-`max` frame
/// before allocating for it.
pub(crate) async fn recv_framed(
    connection: &mut Idevice,
    max: usize,
) -> Result<Vec<u8>, IdeviceError> {
    let header = connection.read_raw(4).await?;
    let len = u32::from_be_bytes([header[0], header[1], header[2], header[3]]) as usize;
    if len == 0 || len > max {
        return Err(IdeviceError::UnexpectedResponse(format!(
            "device framed an implausible {len}-byte message (limit {max})"
        )));
    }
    connection.read_raw(len).await
}

#[cfg(test)]
mod tests {
    use tokio::io::AsyncWriteExt;

    use super::*;

    #[test]
    fn framed_reads_are_bounded_before_allocation() {
        block(async {
            for len in [0_u32, 9] {
                let (device, mut peer) = tokio::io::duplex(64);
                let mut connection = Idevice::new(Box::new(device), "test");
                peer.write_all(&len.to_be_bytes()).await.unwrap();
                assert!(recv_framed(&mut connection, 8).await.is_err(), "{len}");
            }
            let (device, mut peer) = tokio::io::duplex(64);
            let mut connection = Idevice::new(Box::new(device), "test");
            peer.write_all(&8_u32.to_be_bytes()).await.unwrap();
            peer.write_all(b"12345678").await.unwrap();
            assert_eq!(recv_framed(&mut connection, 8).await.unwrap(), b"12345678");
        });
    }
}
