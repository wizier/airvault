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
	for len(message) > 0 {
		key, n := binary.Uvarint(message)
		if n <= 0 {
			return fields
		}
		message = message[n:]
		var skip uint64
		switch key & 7 {
		case 0:
			if _, n = binary.Uvarint(message); n <= 0 {
				return fields
			}
			skip = uint64(n)
		case 1:
			skip = 8
		case 5:
			skip = 4
		case 2:
			size, n := binary.Uvarint(message)
			if n <= 0 || size > uint64(len(message)-n) {
				return fields
			}
			if key>>3 == number {
				fields = append(fields, message[n:n+int(size)])
			}
			skip = uint64(n) + size
		default:
			return fields
		}
		if skip > uint64(len(message)) {
			return fields
		}
		message = message[skip:]
	}
	return fields
}

// protoVarint is the first varint field number of a protobuf message.
func protoVarint(message []byte, number uint64) (uint64, bool) {
	for len(message) > 0 {
		key, n := binary.Uvarint(message)
		if n <= 0 {
			return 0, false
		}
		message = message[n:]
		var skip uint64
		switch key & 7 {
		case 0:
			value, n := binary.Uvarint(message)
			if n <= 0 {
				return 0, false
			}
			if key>>3 == number {
				return value, true
			}
			skip = uint64(n)
		case 1:
			skip = 8
		case 5:
			skip = 4
		case 2:
			size, n := binary.Uvarint(message)
			if n <= 0 || size > uint64(len(message)-n) {
				return 0, false
			}
			skip = uint64(n) + size
		default:
			return 0, false
		}
		if skip > uint64(len(message)) {
			return 0, false
		}
		message = message[skip:]
	}
	return 0, false
}
