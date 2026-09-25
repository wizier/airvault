use idevice::services::afc::opcode::AfcOpcode;
use idevice::services::afc::packet::{AfcPacket, AfcPacketHeader};
use idevice::services::afc::{AfcClient, MAGIC};
use idevice::Idevice;
use tokio::io::{AsyncReadExt, AsyncWriteExt, DuplexStream};
use tokio_util::sync::CancellationToken;

use super::read_small_file;
use crate::bounded::{cancel_or_timeout, Interrupt};
use crate::provider::block;
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

async fn file_info(stream: &mut DuplexStream) {
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

#[test]
fn reuses_only_a_cleanly_closed_session() {
    block(async {
        let (device, mut peer) = tokio::io::duplex(4096);
        let server = tokio::spawn(async move {
            file_info(&mut peer).await;
            file_open(&mut peer).await;

            let (packet_num, opcode) = request(&mut peer).await;
            assert_eq!(opcode, AfcOpcode::Read);
            reply(
                &mut peer,
                packet_num,
                AfcOpcode::Data,
                Vec::new(),
                b"jpeg".to_vec(),
            )
            .await;

            let (packet_num, opcode) = request(&mut peer).await;
            assert_eq!(opcode, AfcOpcode::FileClose);
            reply(
                &mut peer,
                packet_num,
                AfcOpcode::Status,
                0_u64.to_le_bytes().to_vec(),
                Vec::new(),
            )
            .await;
        });
        let client = AfcClient::new(Idevice::new(Box::new(device), "test"));
        let outcome = cancel_or_timeout(
            &CancellationToken::new(),
            timeouts::DEVICE_WORK,
            read_small_file(client, "/thumb", 4),
        )
        .await;
        match outcome {
            Ok(Ok((_, bytes))) => assert_eq!(bytes, b"jpeg"),
            _ => panic!("small read did not return a reusable session"),
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

#[test]
fn cancellation_discards_an_open_descriptor() {
    block(async {
        let (device, mut peer) = tokio::io::duplex(4096);
        let cancel = CancellationToken::new();
        let server_cancel = cancel.clone();
        let server = tokio::spawn(async move {
            file_info(&mut peer).await;
            file_open(&mut peer).await;
            let (_, opcode) = request(&mut peer).await;
            assert_eq!(opcode, AfcOpcode::Read);
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
        assert!(matches!(outcome, Err(Interrupt::Cancelled)));
        drop(server.await.unwrap());
    });
}

#[test]
fn cancellation_interrupts_file_close() {
    block(async {
        let (device, mut peer) = tokio::io::duplex(4096);
        let cancel = CancellationToken::new();
        let server_cancel = cancel.clone();
        let server = tokio::spawn(async move {
            file_info(&mut peer).await;
            file_open(&mut peer).await;

            let (packet_num, opcode) = request(&mut peer).await;
            assert_eq!(opcode, AfcOpcode::Read);
            reply(
                &mut peer,
                packet_num,
                AfcOpcode::Data,
                Vec::new(),
                b"jpeg".to_vec(),
            )
            .await;

            let (_, opcode) = request(&mut peer).await;
            assert_eq!(opcode, AfcOpcode::FileClose);
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
        assert!(matches!(outcome, Err(Interrupt::Cancelled)));
        drop(server.await.unwrap());
    });
}
