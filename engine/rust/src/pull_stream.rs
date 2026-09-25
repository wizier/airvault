//! The owned pull stream behind every av_*_open / next / cancel / close family.
//! A spawned producer reads the device and feeds a channel; `next` only waits on
//! that channel, so a quiet tick can never cut a device message in half.

use std::ffi::c_char;
use std::future::Future;
use std::sync::Mutex;
use std::time::Duration;

use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;

use crate::engine_error::EngineFailure;
use crate::ffi::{write_failure, AV_STREAM_CLOSED, AV_STREAM_CONTINUE};
use crate::provider::{block, spawn};

/// How often `next` returns to Go so it can notice cancellation.
const STREAM_TICK: Duration = Duration::from_secs(1);
/// Bounded, so a slow reader backpressures the device instead of growing memory.
const BUFFERED_ITEMS: usize = 64;

pub(crate) type ItemSender<T> = mpsc::Sender<Result<T, EngineFailure>>;

pub(crate) struct PullStream<T> {
    receiver: Mutex<mpsc::Receiver<Result<T, EngineFailure>>>,
    cancel: CancellationToken,
    task: tokio::task::JoinHandle<()>,
}

impl<T: Send + 'static> PullStream<T> {
    /// Runs `producer` until it returns or the stream is cancelled; its failure
    /// becomes the last item. Cancellation drops it wherever it is waiting.
    pub(crate) fn spawn<F, Fut>(producer: F) -> Self
    where
        F: FnOnce(ItemSender<T>) -> Fut,
        Fut: Future<Output = Result<(), EngineFailure>> + Send + 'static,
    {
        let (sender, receiver) = mpsc::channel(BUFFERED_ITEMS);
        let cancel = CancellationToken::new();
        let producer = producer(sender.clone());
        let stopped = cancel.clone();
        let task = spawn(async move {
            tokio::select! {
                _ = stopped.cancelled() => {}
                result = producer => {
                    if let Err(failure) = result {
                        let _ = sender.send(Err(failure)).await;
                    }
                }
            }
        });
        Self {
            receiver: Mutex::new(receiver),
            cancel,
            task,
        }
    }

    /// One pull: the next item, or the rc to return instead — CONTINUE after a
    /// quiet tick, CLOSED once cancelled or finished, or the producer's failure.
    pub(crate) fn next(&self, err: *mut *mut c_char) -> Result<T, i32> {
        let mut receiver = crate::lock(&self.receiver);
        let pulled = block(async {
            tokio::select! {
                biased;
                _ = self.cancel.cancelled() => Ok(None),
                item = tokio::time::timeout(STREAM_TICK, receiver.recv()) => item,
            }
        });
        match pulled {
            Err(_) => Err(AV_STREAM_CONTINUE),
            Ok(None) => Err(AV_STREAM_CLOSED),
            Ok(Some(Ok(item))) => Ok(item),
            Ok(Some(Err(failure))) => Err(write_failure(err, failure)),
        }
    }

    pub(crate) fn cancel(&self) {
        self.cancel.cancel();
    }

    /// Stops the producer and waits for it to finish.
    pub(crate) fn close(self) {
        let Self {
            receiver,
            cancel,
            task,
        } = self;
        cancel.cancel();
        // Unblocks a producer still delivering its failure into a full channel.
        drop(receiver);
        let _ = block(task);
    }
}
