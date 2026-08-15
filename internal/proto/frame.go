// Package proto implements the framing and message types shared by the control
// clients, the manager and the per-unit supervisors.
//
// The wire format is specified in designs/docs/next/08-control-protocol.md:
//
//	frame := u32 length (big-endian, of everything after this field, max 16 MiB)
//	         u8  type
//	         u8  flags
//	         u16 reserved (0)
//	         payload[length-4]
//
// Both ends always perform full reads and full writes; a short read continues
// rather than truncating (defect D1), the terminator is a typed frame rather
// than a NUL byte (D2), and the size cap is 16 MiB rather than 64 KiB (D3).
package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// ProtocolVersion is the version announced in HELLO and HELLO_ACK. Bump it
// whenever a message type changes meaning.
const ProtocolVersion uint16 = 2

// MaxFrameSize caps a single frame's payload plus header.
const MaxFrameSize = 16 << 20

// headerSize is the size of the fixed part following the length field.
const headerSize = 4

// Frame types. Client<->manager types live below 0x40, manager<->supervisor
// types at 0x40 and above.
const (
	TypeHello     uint8 = 0x01
	TypeHelloAck  uint8 = 0x02
	TypeRequest   uint8 = 0x10
	TypeProgress  uint8 = 0x20
	TypeData      uint8 = 0x21
	TypeLog       uint8 = 0x22
	TypeResult    uint8 = 0x2F
	TypeCancel    uint8 = 0x30
	TypeConfig    uint8 = 0x40
	TypeStart     uint8 = 0x41
	TypeStop      uint8 = 0x42
	TypeReload    uint8 = 0x43
	TypeKill      uint8 = 0x44
	TypeQuery     uint8 = 0x45
	TypeState     uint8 = 0x50
	TypeLogRecord uint8 = 0x51
	TypeNotify    uint8 = 0x52
	TypeExited    uint8 = 0x53
)

// ErrFrameTooLarge is returned when either end sees a length above MaxFrameSize.
var ErrFrameTooLarge = errors.New("frame too large")

// Frame is a single protocol message.
type Frame struct {
	Type    uint8
	Flags   uint8
	Payload []byte
}

// Write serialises f to w, looping until every byte is written.
func Write(w io.Writer, f Frame) error {
	if len(f.Payload)+headerSize > MaxFrameSize {
		return fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, len(f.Payload))
	}
	buf := make([]byte, 8+len(f.Payload))
	binary.BigEndian.PutUint32(buf[0:4], uint32(headerSize+len(f.Payload)))
	buf[4] = f.Type
	buf[5] = f.Flags
	binary.BigEndian.PutUint16(buf[6:8], 0)
	copy(buf[8:], f.Payload)
	_, err := w.Write(buf)
	return err
}

// Read deserialises one frame from r. It uses io.ReadFull throughout, so a
// message split across an arbitrary number of reads is reassembled correctly.
func Read(r io.Reader) (Frame, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return Frame{}, err
	}
	length := binary.BigEndian.Uint32(lenBuf[:])
	if length < headerSize {
		return Frame{}, fmt.Errorf("short frame: length %d", length)
	}
	if int64(length) > MaxFrameSize {
		return Frame{}, fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, length)
	}
	hdr := make([]byte, headerSize)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return Frame{}, err
	}
	f := Frame{Type: hdr[0], Flags: hdr[1]}
	if n := int(length) - headerSize; n > 0 {
		f.Payload = make([]byte, n)
		if _, err := io.ReadFull(r, f.Payload); err != nil {
			return Frame{}, err
		}
	}
	return f, nil
}
