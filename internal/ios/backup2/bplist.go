package backup2

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"time"
	"unicode/utf16"

	"github.com/wizier/airvault/internal/ios"
)

// Dict keeps keys in the order the device sent them: DeviceLink moves files
// in that order.
type Dict struct {
	Keys   []string
	Values []any
}

func (d *Dict) Get(key string) any {
	for i, k := range d.Keys {
		if k == key {
			return d.Values[i]
		}
	}
	return nil
}

var plistEpoch = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

func decodeBinary(data []byte) (any, error) {
	if len(data) < 8+32 || !bytes.HasPrefix(data, []byte("bplist00")) {
		return nil, fmt.Errorf("%w: not a binary plist", ios.ErrProtocol)
	}
	trailer := data[len(data)-32:]
	d := &bplistDecoder{data: data, offsetSize: int(trailer[6]), refSize: int(trailer[7])}
	count := binary.BigEndian.Uint64(trailer[8:])
	top := binary.BigEndian.Uint64(trailer[16:])
	table := binary.BigEndian.Uint64(trailer[24:])
	if d.offsetSize < 1 || d.offsetSize > 8 || d.refSize < 1 || d.refSize > 8 ||
		count == 0 || table > uint64(len(data)-32) || count > (uint64(len(data)-32)-table)/uint64(d.offsetSize) || top >= count {
		return nil, fmt.Errorf("%w: bad binary plist trailer", ios.ErrProtocol)
	}
	d.offsets = data[table : table+count*uint64(d.offsetSize)]
	d.count = count
	return d.object(top, 0)
}

type bplistDecoder struct {
	data                []byte
	offsets             []byte
	count               uint64
	offsetSize, refSize int
	visits              int
	bytes               uint64
}

// Shared objects let a small plist describe a huge tree; these bound the
// work and the memory, counting a shared object each time it is reached.
const (
	maxPlistDepth  = 64
	maxPlistVisits = 1 << 20
	maxPlistBytes  = 64 << 20 // strings and data
)

func (d *bplistDecoder) object(ref uint64, depth int) (any, error) {
	d.visits++
	if ref >= d.count || depth > maxPlistDepth || d.visits > maxPlistVisits {
		return nil, d.bad("object graph")
	}
	offset := sized(d.offsets[ref*uint64(d.offsetSize):], d.offsetSize)
	if offset >= uint64(len(d.data)-32) {
		return nil, d.bad("object offset")
	}
	marker := d.data[offset]
	kind, info := marker>>4, marker&0x0F
	body := d.data[offset+1 : len(d.data)-32]
	switch kind {
	case 0x0:
		switch info {
		case 0x0:
			return nil, nil
		case 0x8:
			return false, nil
		case 0x9:
			return true, nil
		}
	case 0x1:
		size := 1 << info
		if info > 4 || len(body) < size {
			return nil, d.bad("integer")
		}
		// 1, 2 and 4-byte integers are unsigned, 8-byte ones signed; a 16-byte
		// one keeps its low 8 bytes.
		return int64(sized(body[size-min(size, 8):], min(size, 8))), nil
	case 0x2:
		switch {
		case info == 2 && len(body) >= 4:
			return float64(math.Float32frombits(binary.BigEndian.Uint32(body))), nil
		case info == 3 && len(body) >= 8:
			return math.Float64frombits(binary.BigEndian.Uint64(body)), nil
		}
	case 0x3:
		if info == 3 && len(body) >= 8 {
			seconds := math.Float64frombits(binary.BigEndian.Uint64(body))
			return plistEpoch.Add(time.Duration(seconds * float64(time.Second))), nil
		}
	case 0x4, 0x5, 0x6:
		n, body, err := d.length(info, body)
		if err != nil {
			return nil, err
		}
		width := 1
		if kind == 0x6 {
			width = 2
		}
		if n > uint64(len(body)/width) || n*uint64(width) > maxPlistBytes-d.bytes {
			return nil, d.bad("string or data length")
		}
		d.bytes += n * uint64(width)
		content := body[:n*uint64(width)]
		switch kind {
		case 0x4:
			return bytes.Clone(content), nil
		case 0x5:
			return string(content), nil
		}
		units := make([]uint16, n)
		for i := range units {
			units[i] = binary.BigEndian.Uint16(content[2*i:])
		}
		return string(utf16.Decode(units)), nil
	case 0xA, 0xC, 0xD:
		n, body, err := d.length(info, body)
		if err != nil {
			return nil, err
		}
		refs := n
		if kind == 0xD {
			refs *= 2
		}
		if n > uint64(len(body)) || refs > uint64(len(body)/d.refSize) || refs > uint64(maxPlistVisits-d.visits) {
			return nil, d.bad("collection length")
		}
		values := make([]any, refs)
		for i := range values {
			if values[i], err = d.object(sized(body[i*d.refSize:], d.refSize), depth+1); err != nil {
				return nil, err
			}
		}
		if kind != 0xD {
			return values, nil
		}
		dict := &Dict{Keys: make([]string, n), Values: values[n:]}
		for i := range dict.Keys {
			key, ok := values[i].(string)
			if !ok {
				return nil, d.bad("dictionary key")
			}
			dict.Keys[i] = key
		}
		return dict, nil
	}
	return nil, d.bad(fmt.Sprintf("object marker %#x", marker))
}

// length reads a count from the marker, or from the integer after it.
func (d *bplistDecoder) length(info byte, body []byte) (uint64, []byte, error) {
	if info != 0x0F {
		return uint64(info), body, nil
	}
	if len(body) < 1 || body[0]>>4 != 0x1 || body[0]&0x0F > 3 {
		return 0, nil, d.bad("length")
	}
	size := 1 << (body[0] & 0x0F)
	if len(body) < 1+size {
		return 0, nil, d.bad("length")
	}
	return sized(body[1:], size), body[1+size:], nil
}

func (d *bplistDecoder) bad(what string) error {
	return fmt.Errorf("%w: bad binary plist %s", ios.ErrProtocol, what)
}

func sized(b []byte, size int) uint64 {
	var v uint64
	for _, c := range b[:size] {
		v = v<<8 | uint64(c)
	}
	return v
}
