//! Idle AFC connections kept per device and source, so a browsing burst pays
//! the lockdown/TLS/StartService handshake once instead of on every request.
//! Only clients whose protocol state is known idle and healthy come back here.

use std::collections::HashMap;
use std::future::Future;
use std::sync::{Arc, Mutex, Weak};
use std::time::Duration;

use idevice::services::afc::AfcClient;
use idevice::usbmuxd::Connection;
use idevice::IdeviceError;
use tokio::time::Instant;

use crate::afc::Source;

/// Covers the think time of a browsing burst, then releases the phone-side
/// afcd instance and the netmuxd proxy.
const IDLE_TIMEOUT: Duration = Duration::from_secs(30);
/// net/http and database/sql default; a thumbnail batch overlapping a preview.
const MAX_IDLE_PER_KEY: usize = 2;
/// Bounds staleness: an app update replaces its container, trust can be revoked.
const MAX_LIFETIME: Duration = Duration::from_secs(5 * 60);
/// A client returned this recently is reused without a round trip (bursts).
const VALIDATE_AFTER_IDLE: Duration = Duration::from_secs(2);
/// A healthy phone answers one stat quickly; a slow one is cheaper to replace.
const VALIDATE_TIMEOUT: Duration = Duration::from_secs(2);

#[derive(Clone, Debug, PartialEq, Eq, Hash)]
pub(crate) struct PoolKey {
    pub(crate) udid: String,
    pub(crate) source: Source,
}

/// The muxer transport a client was opened over; a client is only reused on
/// the device's current preferred transport.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Transport {
    Usb,
    Network,
    Unknown,
}

impl From<&Connection> for Transport {
    fn from(connection: &Connection) -> Self {
        match connection {
            Connection::Usb => Self::Usb,
            Connection::Network(_) => Self::Network,
            Connection::Unknown(_) => Self::Unknown,
        }
    }
}

/// Where a checked-out client came from; it travels with the client's handle
/// so the client can be returned to the right key when the handle closes.
#[derive(Clone, Debug)]
pub(crate) struct ClientOrigin {
    pool: Weak<AfcPool>,
    key: PoolKey,
    transport: Transport,
    created: Instant,
}

impl ClientOrigin {
    /// Returns a healthy, protocol-idle client to the pool it came from.
    pub(crate) fn check_in(self, client: AfcClient) {
        if let Some(pool) = self.pool.upgrade() {
            pool.put(client, self);
        }
    }
}

#[derive(Debug)]
struct IdleClient {
    id: u64,
    client: AfcClient,
    transport: Transport,
    created: Instant,
    returned: Instant,
}

impl IdleClient {
    fn expired(&self) -> bool {
        self.returned.elapsed() >= IDLE_TIMEOUT || self.created.elapsed() >= MAX_LIFETIME
    }
}

#[derive(Debug, Default)]
struct PoolState {
    next_id: u64,
    // Per key in return order: the last entry is the most recently returned.
    idle: HashMap<PoolKey, Vec<IdleClient>>,
}

#[derive(Debug, Default)]
pub(crate) struct AfcPool {
    state: Mutex<PoolState>,
}

impl AfcPool {
    pub(crate) fn new() -> Arc<Self> {
        Arc::new(Self::default())
    }

    /// Hands out the most recently returned idle client for `key` on
    /// `transport`, validated when it sat idle past the bypass window, or
    /// else a client from `connect`.
    pub(crate) async fn checkout(
        self: &Arc<Self>,
        key: &PoolKey,
        transport: Transport,
        connect: impl Future<Output = Result<AfcClient, IdeviceError>>,
    ) -> Result<(AfcClient, ClientOrigin), IdeviceError> {
        let udid = key.udid.as_str();
        if let Some(mut idle) = self.take_idle(key, transport) {
            let origin = self.origin(key, transport, idle.created);
            if idle.returned.elapsed() < VALIDATE_AFTER_IDLE {
                tracing::debug!(udid, "AFC connection reused");
                return Ok((idle.client, origin));
            }
            let probe = tokio::time::timeout(VALIDATE_TIMEOUT, idle.client.get_file_info("/"));
            if let Ok(Ok(_)) = probe.await {
                tracing::debug!(udid, "AFC connection validated and reused");
                return Ok((idle.client, origin));
            }
            tracing::debug!(udid, "AFC connection failed validation; dropped");
        }
        let client = connect.await?;
        tracing::debug!(udid, ?transport, "AFC connection opened fresh");
        Ok((client, self.origin(key, transport, Instant::now())))
    }

    /// Drops every idle client for `udid`, e.g. after its pairing record changed.
    pub(crate) fn forget(&self, udid: &str) {
        let mut state = crate::lock(&self.state);
        let before = state.idle.len();
        state.idle.retain(|key, _| key.udid != udid);
        if state.idle.len() != before {
            tracing::debug!(udid, "idle AFC connections forgotten");
        }
    }

    fn origin(
        self: &Arc<Self>,
        key: &PoolKey,
        transport: Transport,
        created: Instant,
    ) -> ClientOrigin {
        ClientOrigin {
            pool: Arc::downgrade(self),
            key: key.clone(),
            transport,
            created,
        }
    }

    /// The newest live idle client for `key` on `transport`. Expired entries and
    /// entries on another transport (e.g. Wi-Fi once the cable is in) are dropped.
    fn take_idle(&self, key: &PoolKey, transport: Transport) -> Option<IdleClient> {
        let mut state = crate::lock(&self.state);
        let entries = state.idle.get_mut(key)?;
        entries.retain(|entry| {
            let reason = if entry.transport != transport {
                "on another transport"
            } else if entry.expired() {
                "expired"
            } else {
                return true;
            };
            tracing::debug!(udid = %key.udid, reason, "idle AFC connection dropped");
            false
        });
        let entry = entries.pop();
        if entries.is_empty() {
            state.idle.remove(key);
        }
        entry
    }

    /// Keeps a returned client as the newest idle entry for its key until its
    /// idle timer fires; the oldest entry beyond the cap is closed.
    fn put(&self, client: AfcClient, origin: ClientOrigin) {
        let udid = origin.key.udid.as_str();
        if origin.created.elapsed() >= MAX_LIFETIME {
            tracing::debug!(udid, "AFC connection reached its lifetime; closed");
            return;
        }
        let id = {
            let mut state = crate::lock(&self.state);
            state.next_id += 1;
            let id = state.next_id;
            let entries = state.idle.entry(origin.key.clone()).or_default();
            entries.push(IdleClient {
                id,
                client,
                transport: origin.transport,
                created: origin.created,
                returned: Instant::now(),
            });
            if entries.len() > MAX_IDLE_PER_KEY {
                entries.remove(0);
            }
            id
        };
        // The timer holds only a weak handle, so a closed engine drops its
        // clients at once instead of when the last timer fires.
        tokio::spawn(async move {
            tokio::time::sleep(IDLE_TIMEOUT).await;
            if let Some(pool) = origin.pool.upgrade() {
                pool.expire(&origin.key, id);
            }
        });
    }

    fn expire(&self, key: &PoolKey, id: u64) {
        let mut state = crate::lock(&self.state);
        let Some(entries) = state.idle.get_mut(key) else {
            return;
        };
        let before = entries.len();
        entries.retain(|entry| entry.id != id);
        if entries.len() != before {
            tracing::debug!(udid = %key.udid, "idle AFC connection expired; closed");
        }
        if entries.is_empty() {
            state.idle.remove(key);
        }
    }

    #[cfg(test)]
    pub(crate) fn idle_count(&self, key: &PoolKey) -> usize {
        crate::lock(&self.state).idle.get(key).map_or(0, Vec::len)
    }
}

#[cfg(test)]
mod tests {
    use std::sync::atomic::{AtomicBool, Ordering};

    use idevice::Idevice;
    use tokio::io::{AsyncReadExt, DuplexStream};

    use super::*;
    use crate::afc::lifecycle_tests::file_info;

    fn key(udid: &str) -> PoolKey {
        PoolKey {
            udid: udid.to_owned(),
            source: Source::Media,
        }
    }

    fn client() -> (AfcClient, DuplexStream) {
        let (device, peer) = tokio::io::duplex(4096);
        (AfcClient::new(Idevice::new(Box::new(device), "test")), peer)
    }

    /// Returns one idle client for `key` to the pool; the peer sees its socket.
    fn seed(pool: &Arc<AfcPool>, key: &PoolKey, transport: Transport) -> DuplexStream {
        let (idle, peer) = client();
        pool.origin(key, transport, Instant::now()).check_in(idle);
        peer
    }

    /// Checks out a client for `key`, reporting whether a fresh connect ran.
    async fn checkout(
        pool: &Arc<AfcPool>,
        key: &PoolKey,
        transport: Transport,
    ) -> (AfcClient, ClientOrigin, bool) {
        let connected = AtomicBool::new(false);
        let connect = async {
            connected.store(true, Ordering::SeqCst);
            Ok(client().0)
        };
        let (client, origin) = pool.checkout(key, transport, connect).await.unwrap();
        (client, origin, connected.load(Ordering::SeqCst))
    }

    #[tokio::test(start_paused = true)]
    async fn a_recent_client_is_reused_without_a_round_trip() {
        let pool = AfcPool::new();
        // A dead peer would fail any validation round trip.
        drop(seed(&pool, &key("a"), Transport::Usb));
        let (_, _, connected) = checkout(&pool, &key("a"), Transport::Usb).await;
        assert!(!connected);
        assert_eq!(pool.idle_count(&key("a")), 0);
    }

    #[tokio::test(start_paused = true)]
    async fn an_older_client_is_validated_and_a_failed_one_replaced() {
        let pool = AfcPool::new();
        let mut peer = seed(&pool, &key("a"), Transport::Usb);
        tokio::time::advance(VALIDATE_AFTER_IDLE).await;
        let server = tokio::spawn(async move {
            file_info(&mut peer).await;
            peer
        });
        let (validated, origin, connected) = checkout(&pool, &key("a"), Transport::Usb).await;
        assert!(!connected);

        origin.check_in(validated);
        tokio::time::advance(VALIDATE_AFTER_IDLE).await;
        drop(server.await.unwrap());
        let (_, _, connected) = checkout(&pool, &key("a"), Transport::Usb).await;
        assert!(connected);
    }

    #[tokio::test(start_paused = true)]
    async fn an_idle_client_is_closed_when_its_timer_fires() {
        let pool = AfcPool::new();
        let start = Instant::now();
        let mut peer = seed(&pool, &key("a"), Transport::Usb);
        tokio::time::advance(IDLE_TIMEOUT - Duration::from_millis(1)).await;
        assert_eq!(pool.idle_count(&key("a")), 1);
        assert_eq!(peer.read(&mut [0; 1]).await.unwrap(), 0, "socket closed");
        assert!(start.elapsed() >= IDLE_TIMEOUT);
        assert_eq!(pool.idle_count(&key("a")), 0);
    }

    #[tokio::test(start_paused = true)]
    async fn put_enforces_the_idle_cap_and_the_lifetime() {
        let pool = AfcPool::new();
        let mut oldest = seed(&pool, &key("a"), Transport::Usb);
        let _newer = [
            seed(&pool, &key("a"), Transport::Usb),
            seed(&pool, &key("a"), Transport::Usb),
        ];
        assert_eq!(pool.idle_count(&key("a")), MAX_IDLE_PER_KEY);
        assert_eq!(oldest.read(&mut [0; 1]).await.unwrap(), 0, "oldest closed");

        let origin = pool.origin(&key("b"), Transport::Usb, Instant::now());
        tokio::time::advance(MAX_LIFETIME).await;
        origin.check_in(client().0);
        assert_eq!(pool.idle_count(&key("b")), 0);
    }

    #[tokio::test(start_paused = true)]
    async fn a_client_on_another_transport_is_discarded() {
        let pool = AfcPool::new();
        let _peer = seed(&pool, &key("a"), Transport::Network);
        let (_, _, connected) = checkout(&pool, &key("a"), Transport::Usb).await;
        assert!(connected);
        assert_eq!(pool.idle_count(&key("a")), 0);
    }

    #[tokio::test(start_paused = true)]
    async fn forget_clears_only_that_device() {
        let pool = AfcPool::new();
        let documents = PoolKey {
            udid: "a".to_owned(),
            source: Source::AppDocuments("com.example".to_owned()),
        };
        let _peers = [
            seed(&pool, &key("a"), Transport::Usb),
            seed(&pool, &documents, Transport::Usb),
            seed(&pool, &key("b"), Transport::Usb),
        ];
        pool.forget("a");
        assert_eq!(pool.idle_count(&key("a")), 0);
        assert_eq!(pool.idle_count(&documents), 0);
        assert_eq!(pool.idle_count(&key("b")), 1);
    }
}
