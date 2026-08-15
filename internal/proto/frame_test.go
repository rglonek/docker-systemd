package proto

import (
	"bytes"
	"io"
	"testing"
)

// slowReader hands out at most n bytes per Read, so a message is guaranteed to
// arrive split across many reads.
type slowReader struct {
	data []byte
	n    int
}

func (r *slowReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := r.n
	if n > len(p) {
		n = len(p)
	}
	if n > len(r.data) {
		n = len(r.data)
	}
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

// Defect D1: both ends assumed a single read() returned a whole message.
func TestFrameSplitAcrossManyReads(t *testing.T) {
	payload := bytes.Repeat([]byte("abcdefghij"), 1000)
	var buf bytes.Buffer
	if err := Write(&buf, Frame{Type: TypeData, Payload: payload}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Ten bytes at a time: a 10 KB payload arrives in ~1000 reads.
	r := &slowReader{data: buf.Bytes(), n: 10}
	f, err := Read(r)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if f.Type != TypeData {
		t.Errorf("Type = %#x", f.Type)
	}
	if !bytes.Equal(f.Payload, payload) {
		t.Error("payload was corrupted by the split reads")
	}
}

// Defect D2: the terminator was a bare 0x00, so unit output containing a NUL
// truncated the stream.
func TestPayloadContainingNUL(t *testing.T) {
	payload := []byte("before\x00after\x00\x00end")
	var buf bytes.Buffer
	if err := Write(&buf, Frame{Type: TypeProgress, Payload: payload}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	f, err := Read(&buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(f.Payload, payload) {
		t.Errorf("payload = %q; want %q", f.Payload, payload)
	}
}

// Defect D3: a hard 64 KiB cap on both directions.
func TestLargeFrameRoundTrips(t *testing.T) {
	payload := bytes.Repeat([]byte{'x'}, 4<<20) // 4 MiB
	var buf bytes.Buffer
	if err := Write(&buf, Frame{Type: TypeData, Payload: payload}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	f, err := Read(&buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(f.Payload) != len(payload) {
		t.Errorf("got %d bytes; want %d", len(f.Payload), len(payload))
	}
}

func TestOversizeFrameRejected(t *testing.T) {
	oversize := make([]byte, MaxFrameSize)
	var buf bytes.Buffer
	if err := Write(&buf, Frame{Type: TypeData, Payload: oversize}); err == nil {
		t.Error("writing a frame above the cap should fail")
	}

	// A declared length above the cap must be rejected without allocating it.
	hdr := []byte{0xFF, 0xFF, 0xFF, 0xFF, TypeData, 0, 0, 0}
	if _, err := Read(bytes.NewReader(hdr)); err == nil {
		t.Error("reading an oversize length should fail")
	}
}

func TestShortLengthRejected(t *testing.T) {
	hdr := []byte{0, 0, 0, 1}
	if _, err := Read(bytes.NewReader(hdr)); err == nil {
		t.Error("a length below the header size should be rejected")
	}
}

func TestEmptyPayload(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, Frame{Type: TypeQuery}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	f, err := Read(&buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if f.Type != TypeQuery || len(f.Payload) != 0 {
		t.Errorf("got %+v", f)
	}
}

func TestSequentialFrames(t *testing.T) {
	var buf bytes.Buffer
	types := []uint8{TypeProgress, TypeProgress, TypeData, TypeResult}
	for i, ty := range types {
		if err := Write(&buf, Frame{Type: ty, Payload: []byte{byte(i)}}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	for i, ty := range types {
		f, err := Read(&buf)
		if err != nil {
			t.Fatalf("Read %d: %v", i, err)
		}
		if f.Type != ty || f.Payload[0] != byte(i) {
			t.Errorf("frame %d = %+v", i, f)
		}
	}
	if _, err := Read(&buf); err != io.EOF {
		t.Errorf("expected EOF after the last frame, got %v", err)
	}
}

func TestJSONRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	req := Request{Verb: "start", Units: []string{"nginx.service"}}
	req.Options.Quiet = true
	if err := WriteJSON(&buf, TypeRequest, req); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	f, err := Read(&buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var got Request
	if err := ReadJSON(f, &got); err != nil {
		t.Fatalf("ReadJSON: %v", err)
	}
	if got.Verb != "start" || len(got.Units) != 1 || !got.Options.Quiet {
		t.Errorf("round trip lost data: %+v", got)
	}
}

func FuzzReadFrame(f *testing.F) {
	var buf bytes.Buffer
	_ = Write(&buf, Frame{Type: TypeData, Payload: []byte("hello")})
	f.Add(buf.Bytes())
	f.Add([]byte{0, 0, 0, 4, 1, 0, 0, 0})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		// Reading arbitrary bytes must never panic or allocate unboundedly.
		_, _ = Read(bytes.NewReader(data))
	})
}
