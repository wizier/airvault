package backup2

import (
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
	"time"

	"howett.net/plist"

	"github.com/wizier/airvault/internal/ios"
)

func TestDecodeBinaryMatchesReference(t *testing.T) {
	value := []any{
		"DLMessageMoveFiles",
		map[string]any{
			"ascii":   "Manifest.db",
			"unicode": "Фото ☃",
			"small":   int64(7),
			"wide":    int64(1 << 40),
			"signed":  int64(-12),
			"real":    2.5,
			"flag":    true,
			"data":    []byte{0, 1, 2},
			"date":    time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC),
			"nested":  map[string]any{"list": []any{"a", int64(1), false}},
		},
	}
	body, err := plist.Marshal(value, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeBinary(body)
	if err != nil {
		t.Fatal(err)
	}
	var reference any
	if _, err := plist.Unmarshal(body, &reference); err != nil {
		t.Fatal(err)
	}
	if want := ordered(reference); !reflect.DeepEqual(got, want) {
		t.Errorf("decoded\n%#v\nwant\n%#v", got, want)
	}
}

// bplistOf encodes objects, given as marker and body, with the first on top.
func bplistOf(objects ...[]byte) []byte {
	data := []byte("bplist00")
	var offsets []byte
	for _, object := range objects {
		offsets = append(offsets, byte(len(data)))
		data = append(data, object...)
	}
	trailer := make([]byte, 32)
	trailer[6], trailer[7] = 1, 1
	binary.BigEndian.PutUint64(trailer[8:], uint64(len(objects)))
	binary.BigEndian.PutUint64(trailer[24:], uint64(len(data)))
	return append(append(data, offsets...), trailer...)
}

func ascii(s string) []byte { return append([]byte{0x50 | byte(len(s))}, s...) }

func TestDecodeBinaryKeepsDictionaryOrder(t *testing.T) {
	body := bplistOf([]byte{0xD2, 1, 2, 3, 4}, ascii("b"), ascii("a"), ascii("1"), ascii("2"))
	got, err := decodeBinary(body)
	if err != nil {
		t.Fatal(err)
	}
	want := &Dict{Keys: []string{"b", "a"}, Values: []any{"1", "2"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decoded %#v, want %#v", got, want)
	}
}

func TestDecodeBinaryRejectsHostileInput(t *testing.T) {
	// Each level lists the next one 14 times: small, and a huge tree.
	var fanOut [][]byte
	for level := range 10 {
		object := []byte{0xAE}
		for range 14 {
			object = append(object, byte(level+1))
		}
		fanOut = append(fanOut, object)
	}
	fanOut = append(fanOut, ascii("x"))
	hugeString := append([]byte{0x5F, 0x13}, binary.BigEndian.AppendUint64(nil, 1<<62)...)
	// One array claiming more elements than the decoder visits in total.
	longArray := append([]byte{0xAF, 0x12}, binary.BigEndian.AppendUint32(nil, maxPlistVisits+1)...)
	longArray = append(longArray, make([]byte, maxPlistVisits+1)...)
	// One 100 KiB data object reached a thousand times.
	sharedData := append([]byte{0x4F, 0x12}, binary.BigEndian.AppendUint32(nil, 100<<10)...)
	sharedData = append(sharedData, make([]byte, 100<<10)...)
	manyRefs := append([]byte{0xAF, 0x11}, binary.BigEndian.AppendUint16(nil, 1000)...)
	for range 1000 {
		manyRefs = append(manyRefs, 1)
	}
	cases := map[string][]byte{
		"cycle":          bplistOf([]byte{0xA1, 0}),
		"shared fan-out": bplistOf(fanOut...),
		"huge length":    bplistOf(hugeString),
		"long array":     bplistOf(longArray),
		"shared data":    bplistOf(manyRefs, sharedData),
		"reference out":  bplistOf([]byte{0xA1, 9}),
		"non-string key": bplistOf([]byte{0xD1, 1, 1}, []byte{0x10, 1}),
		"truncated":      bplistOf([]byte{0x13, 0, 0}),
		"unknown marker": bplistOf([]byte{0x80, 0}),
		"not a plist":    []byte("bplist00"),
		"table off the end": func() []byte {
			body := bplistOf(ascii("x"))
			binary.BigEndian.PutUint64(body[len(body)-8:], 1<<40)
			return body
		}(),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeBinary(body); !errors.Is(err, ios.ErrProtocol) {
				t.Errorf("err = %v, want ErrProtocol", err)
			}
		})
	}
}

func FuzzDecodeBinary(f *testing.F) {
	f.Add(bplistOf([]byte{0xD2, 1, 2, 3, 4}, ascii("b"), ascii("a"), ascii("1"), ascii("2")))
	f.Add(bplistOf([]byte{0xA2, 1, 1}, []byte{0x33, 0, 0, 0, 0, 0, 0, 0, 0}))
	if body, err := plist.Marshal([]any{"DLMessageUploadFiles", map[string]any{}, 50.0, uint64(3)}, plist.BinaryFormat); err == nil {
		f.Add(body)
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		_, _ = decodeBinary(body)
	})
}
