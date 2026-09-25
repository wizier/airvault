use std::ptr;
use std::sync::Arc;

use idevice::services::afc::opcode::{AfcFopenMode, AfcOpcode};
use idevice::services::afc::packet::{AfcPacket, AfcPacketHeader};
use idevice::services::afc::{AfcClient, MAGIC};
use idevice::Idevice;
use tokio::io::{AsyncReadExt, AsyncWriteExt, DuplexStream};
use tokio_util::sync::CancellationToken;

use super::{
    av_afc_file_cancel, av_afc_file_close, av_afc_file_read, read_small_file, AvAfcFile, FileGuard,
    Resource, Slot, Source,
};
use crate::afc_pool::{AfcPool, PoolKey, Transport};
use crate::bounded::{cancel_or_timeout, Interrupt};
use crate::engine_error::ErrorKind;
use crate::ffi::{av_buffer_free, AvBuffer, AvError};
use crate::provider::{block, spawn};
use crate::timeouts;

async fn request(stream: &mut DuplexStream) -> (u64, AfcOpcode) {
    let mut header = [0_u8; AfcPacketHeader::LEN as usize];
    stream.read_exact(&mut header).await.unwrap();
    let entire_len = u64::from_le_bytes(header[8..16].try_into().unwrap()) as usize;
    let packet_num = u64::from_le_bytes(header[24..32].try_into().unwrap());
    let opcode =
        AfcOpcode::try_from(u64::from_le_bytes(header[32..40].try_into().unwrap())).unwrap();
    let mut rest = vec![0; entire_len - header.len()];
    stream.read_exact(&mut rest).await.unwrap();
    (packet_num, opcode)
}

async fn reply(
    stream: &mut DuplexStream,
    packet_num: u64,
    opcode: AfcOpcode,
    header_payload: Vec<u8>,
    payload: Vec<u8>,
) {
    let header_payload_len = AfcPacketHeader::LEN + header_payload.len() as u64;
    let packet = AfcPacket {
        header: AfcPacketHeader {
            magic: MAGIC,
            entire_len: header_payload_len + payload.len() as u64,
            header_payload_len,
            packet_num,
            operation: opcode,
        },
        header_payload,
        payload,
    };
    stream.write_all(&packet.serialize()).await.unwrap();
}

pub(crate) async fn file_info(stream: &mut DuplexStream) {
    let (packet_num, opcode) = request(stream).await;
    assert_eq!(opcode, AfcOpcode::GetFileInfo);
    reply(
        stream,
        packet_num,
        AfcOpcode::Data,
        Vec::new(),
        b"st_size\x004\x00st_blocks\x001\x00st_birthtime\x000\x00st_mtime\x000\x00st_nlink\x001\x00st_ifmt\x00S_IFREG\x00"
            .to_vec(),
    )
    .await;
}

async fn file_open(stream: &mut DuplexStream) {
    let (packet_num, opcode) = request(stream).await;
    assert_eq!(opcode, AfcOpcode::FileOpen);
    reply(
        stream,
        packet_num,
        AfcOpcode::FileOpenRes,
        7_u64.to_le_bytes().to_vec(),
        Vec::new(),
    )
    .await;
}

async fn file_read(stream: &mut DuplexStream) {
    let (packet_num, opcode) = request(stream).await;
    assert_eq!(opcode, AfcOpcode::Read);
    reply(
        stream,
        packet_num,
        AfcOpcode::Data,
        Vec::new(),
        b"jpeg".to_vec(),
    )
    .await;
}

async fn file_close(stream: &mut DuplexStream) {
    let (packet_num, opcode) = request(stream).await;
    assert_eq!(opcode, AfcOpcode::FileClose);
    reply(
        stream,
        packet_num,
        AfcOpcode::Status,
        0_u64.to_le_bytes().to_vec(),
        Vec::new(),
    )
    .await;
}

#[test]
fn reuses_only_a_cleanly_closed_session() {
    block(async {
        let (device, mut peer) = tokio::io::duplex(4096);
        let server = tokio::spawn(async move {
            file_info(&mut peer).await;
            file_open(&mut peer).await;
            file_read(&mut peer).await;
            file_close(&mut peer).await;
        });
        let client = AfcClient::new(Idevice::new(Box::new(device), "test"));
        match read_small_file(client, "/thumb", 4).await {
            Ok((_, bytes)) => assert_eq!(bytes, b"jpeg"),
            Err(_) => panic!("small read did not return a reusable session"),
        }
        server.await.unwrap();
    });
}

#[test]
fn a_missing_file_keeps_the_session() {
    block(async {
        let (device, mut peer) = tokio::io::duplex(4096);
        let server = tokio::spawn(async move {
            let (packet_num, opcode) = request(&mut peer).await;
            assert_eq!(opcode, AfcOpcode::GetFileInfo);
            let object_not_found = 8_u64.to_le_bytes().to_vec();
            reply(
                &mut peer,
                packet_num,
                AfcOpcode::Status,
                object_not_found,
                Vec::new(),
            )
            .await;
            peer
        });
        let client = AfcClient::new(Idevice::new(Box::new(device), "test"));
        let outcome = read_small_file(client, "/missing", 4).await;
        assert!(matches!(outcome, Err((Some(_), _))));
        drop(server.await.unwrap());
    });
}

// The device never answers the request cancelled at, so only cancellation can
// end the read: an open descriptor is discarded, a pending close is abandoned.
#[test]
fn cancellation_interrupts_an_open_file_at_read_and_close() {
    for cancel_at in [AfcOpcode::Read, AfcOpcode::FileClose] {
        block(async move {
            let (device, mut peer) = tokio::io::duplex(4096);
            let cancel = CancellationToken::new();
            let server_cancel = cancel.clone();
            let server = tokio::spawn(async move {
                file_info(&mut peer).await;
                file_open(&mut peer).await;
                if cancel_at == AfcOpcode::FileClose {
                    file_read(&mut peer).await;
                }
                let (_, opcode) = request(&mut peer).await;
                assert_eq!(opcode, cancel_at);
                server_cancel.cancel();
                peer
            });
            let client = AfcClient::new(Idevice::new(Box::new(device), "test"));
            let outcome = cancel_or_timeout(
                &cancel,
                timeouts::DEVICE_WORK,
                read_small_file(client, "/thumb", 4),
            )
            .await;
            assert!(
                matches!(outcome, Err(Interrupt::Cancelled)),
                "{cancel_at:?}"
            );
            drop(server.await.unwrap());
        });
    }
}

fn media_key() -> PoolKey {
    PoolKey {
        udid: "test".to_owned(),
        source: Source::Media,
    }
}

/// An open file handle like av_afc_file_open produces, checked out of `pool`;
/// the returned peer is the fake device after it answered FileOpen.
fn open_file(pool: &Arc<AfcPool>) -> (*mut AvAfcFile, DuplexStream) {
    block(async {
        let (device, mut peer) = tokio::io::duplex(4096);
        let client = AfcClient::new(Idevice::new(Box::new(device), "test"));
        let connect = async { Ok(client) };
        let (client, origin) = pool
            .checkout(&media_key(), Transport::Usb, connect)
            .await
            .unwrap();
        let (opened, ()) = tokio::join!(
            client.open_owned("/file", AfcFopenMode::RdOnly),
            file_open(&mut peer)
        );
        let slot = Arc::new(Slot::new(Resource::File(FileGuard::new(opened.unwrap()))));
        (Box::into_raw(Box::new(AvAfcFile { slot, origin })), peer)
    })
}

fn read(file: *mut AvAfcFile) -> i32 {
    let mut buffer = [0_u8; 4];
    let mut read = 0;
    let mut error = AvError {
        detail: AvBuffer {
            ptr: ptr::null_mut(),
            len: 0,
        },
    };
    let rc = av_afc_file_read(
        file,
        buffer.as_mut_ptr(),
        buffer.len(),
        &mut read,
        &mut error,
    );
    unsafe { av_buffer_free(error.detail) };
    rc
}

// Go's close order: cancel, wait for the call in flight, destroy.
fn close(file: *mut AvAfcFile) {
    unsafe {
        av_afc_file_cancel(file);
        av_afc_file_close(file);
    }
}

#[test]
fn a_cleanly_closed_file_returns_its_client_to_the_pool() {
    let pool = AfcPool::new();
    let (file, mut peer) = open_file(&pool);
    let server = spawn(async move {
        file_read(&mut peer).await;
        file_close(&mut peer).await;
        peer
    });
    assert_eq!(read(file), 0);
    close(file);
    assert_eq!(pool.idle_count(&media_key()), 1);
    drop(block(server).unwrap());
}

#[test]
fn a_file_interrupted_mid_read_is_not_pooled() {
    let pool = AfcPool::new();
    let (file, mut peer) = open_file(&pool);
    let (read_seen, read_started) = std::sync::mpsc::channel();
    // The device never answers the read, so only cancellation can end it.
    let server = spawn(async move {
        let (_, opcode) = request(&mut peer).await;
        assert_eq!(opcode, AfcOpcode::Read);
        read_seen.send(()).unwrap();
        peer
    });
    let address = file as usize;
    let reader = std::thread::spawn(move || read(address as *mut AvAfcFile));
    read_started.recv().unwrap();
    unsafe { av_afc_file_cancel(file) };
    assert_eq!(reader.join().unwrap(), ErrorKind::Cancelled.code());
    close(file);
    assert_eq!(pool.idle_count(&media_key()), 0);
    drop(block(server).unwrap());
}
