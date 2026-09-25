//! In-flight operation ownership and cancellation.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};

use tokio_util::sync::CancellationToken;

type Entries<T> = Mutex<HashMap<String, Arc<T>>>;

#[derive(Default)]
pub(crate) struct OperationRegistry {
    transfers: Entries<Transfer>,
    commands: Entries<CancellationToken>,
}

pub(crate) struct Transfer {
    udid: String,
    cancel: CancellationToken,
}

#[derive(Debug)]
pub(crate) enum RegisterError {
    DuplicateOperation,
    DeviceBusy,
}

impl OperationRegistry {
    pub(crate) fn register_transfer(
        &self,
        operation_id: String,
        udid: String,
    ) -> Result<Lease<'_, Transfer>, RegisterError> {
        let transfer = Arc::new(Transfer {
            udid,
            cancel: CancellationToken::new(),
        });
        let mut transfers = crate::lock(&self.transfers);
        if transfers.contains_key(&operation_id) {
            return Err(RegisterError::DuplicateOperation);
        }
        if transfers
            .values()
            .any(|current| current.udid == transfer.udid)
        {
            return Err(RegisterError::DeviceBusy);
        }
        transfers.insert(operation_id.clone(), transfer.clone());
        drop(transfers);
        Ok(Lease {
            key: operation_id,
            cancel: transfer.cancel.clone(),
            entry: transfer,
            entries: &self.transfers,
        })
    }

    /// Command registration intentionally replaces the same key. Arc identity
    /// prevents an older lease from removing its successor.
    pub(crate) fn register_command(&self, operation_id: String) -> Lease<'_, CancellationToken> {
        let cancel = Arc::new(CancellationToken::new());
        crate::lock(&self.commands).insert(operation_id.clone(), cancel.clone());
        Lease {
            key: operation_id,
            cancel: (*cancel).clone(),
            entry: cancel,
            entries: &self.commands,
        }
    }

    pub(crate) fn cancel(&self, operation_id: &str) -> bool {
        let transfer = crate::lock(&self.transfers)
            .get(operation_id)
            .map(|transfer| transfer.cancel.clone());
        if let Some(cancel) = transfer {
            cancel.cancel();
            return true;
        }

        let command = crate::lock(&self.commands).get(operation_id).cloned();
        if let Some(cancel) = command {
            cancel.cancel();
            return true;
        }
        false
    }
}

/// Owns one registry entry. Drop is the only removal path, so return and panic
/// unwind clean up identically.
#[must_use = "dropping the lease unregisters the in-flight operation"]
pub(crate) struct Lease<'a, T> {
    key: String,
    cancel: CancellationToken,
    entry: Arc<T>,
    entries: &'a Entries<T>,
}

impl<T> Lease<'_, T> {
    pub(crate) fn cancellation_token(&self) -> CancellationToken {
        self.cancel.clone()
    }
}

impl<T> Drop for Lease<'_, T> {
    fn drop(&mut self) {
        let mut entries = crate::lock(self.entries);
        if entries
            .get(&self.key)
            .is_some_and(|current| Arc::ptr_eq(current, &self.entry))
        {
            entries.remove(&self.key);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::{OperationRegistry, RegisterError};

    #[test]
    fn a_transfer_needs_a_free_device_and_a_fresh_id() {
        let registry = OperationRegistry::default();
        let _lease = registry
            .register_transfer("op".into(), "udid".into())
            .unwrap();
        assert!(matches!(
            registry.register_transfer("op-2".into(), "udid".into()),
            Err(RegisterError::DeviceBusy)
        ));
        assert!(matches!(
            registry.register_transfer("op".into(), "other-udid".into()),
            Err(RegisterError::DuplicateOperation)
        ));
    }

    #[test]
    fn a_dropped_transfer_lease_releases_its_id_and_device() {
        let registry = OperationRegistry::default();
        drop(
            registry
                .register_transfer("op".into(), "udid".into())
                .unwrap(),
        );
        assert!(!registry.cancel("op"));
        assert!(registry
            .register_transfer("op".into(), "udid".into())
            .is_ok());
    }

    #[test]
    fn cancel_reaches_transfers_and_commands() {
        let registry = OperationRegistry::default();
        let transfer = registry
            .register_transfer("op".into(), "udid".into())
            .unwrap();
        let command = registry.register_command("job".into());

        assert!(registry.cancel("op"));
        assert!(registry.cancel("job"));
        assert!(!registry.cancel("unknown"));
        assert!(transfer.cancellation_token().is_cancelled());
        assert!(command.cancellation_token().is_cancelled());
    }

    #[test]
    fn a_stale_command_lease_keeps_its_successor_registered() {
        let registry = OperationRegistry::default();
        let stale = registry.register_command("job".into());
        let current = registry.register_command("job".into());
        drop(stale);

        assert!(registry.cancel("job"));
        assert!(current.cancellation_token().is_cancelled());
    }
}
