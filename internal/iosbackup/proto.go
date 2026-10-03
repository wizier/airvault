package iosbackup

import "encoding/binary"

// protoField is the first length-delimited field number of a protobuf
// message, nil when it has none.
func protoField(message []byte, number uint64) []byte {
	if fields := protoFields(message, number); len(fields) > 0 {
		return fields[0]
	}
	return nil
}

// protoFields is every length-delimited field number of a protobuf message,
// up to where it stops parsing.
func protoFields(message []byte, number uint64) [][]byte {
	var fields [][]byte
	protoWalk(message, func(field, wire, _ uint64, data []byte) bool {
		if field == number && wire == 2 {
			fields = append(fields, data)
		}
		return true
	})
	return fields
}

// protoVarint is the first varint field number of a protobuf message.
func protoVarint(message []byte, number uint64) (value uint64, ok bool) {
	protoWalk(message, func(field, wire, varint uint64, _ []byte) bool {
		if field == number && wire == 0 {
			value, ok = varint, true
		}
		return !ok
	})
	return value, ok
}

// protoWalk calls each with every field of a protobuf message, its number and
// wire type, a varint's value or a length-delimited field's bytes, until each
// returns false or the message stops parsing.
func protoWalk(message []byte, each func(field, wire, varint uint64, data []byte) bool) {
	for len(message) > 0 {
		key, n := binary.Uvarint(message)
		if n <= 0 {
			return
		}
		message = message[n:]
		var varint uint64
		var data []byte
		switch key & 7 {
		case 0:
			if varint, n = binary.Uvarint(message); n <= 0 {
				return
			}
		case 1:
			n = 8
		case 5:
			n = 4
		case 2:
			size, m := binary.Uvarint(message)
			if m <= 0 || size > uint64(len(message)-m) {
				return
			}
			data, n = message[m:m+int(size)], m+int(size)
		default:
			return
		}
		if n > len(message) || !each(key>>3, key&7, varint, data) {
			return
		}
		message = message[n:]
	}
}
