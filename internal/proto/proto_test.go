package proto

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

func TestMsgRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := Hello{V: Version, Token: "zk_a_b", Type: TypeHTTP, Subdomain: "api-x", LocalPort: 3000}
	if err := WriteMsg(&buf, want); err != nil {
		t.Fatalf("WriteMsg: %v", err)
	}
	var got Hello
	if err := ReadMsg(&buf, &got); err != nil {
		t.Fatalf("ReadMsg: %v", err)
	}
	if got != want {
		t.Errorf("round trip mismatch\n got %+v\nwant %+v", got, want)
	}
}

func TestMsgSequenceOnOneStream(t *testing.T) {
	// The event stream sends many frames over one connection, so the reader must
	// consume exactly one frame at a time.
	var buf bytes.Buffer
	for i := range 3 {
		if err := WriteMsg(&buf, Event{T: EventRequest, Status: 200 + i, At: time.Unix(0, 0).UTC()}); err != nil {
			t.Fatalf("WriteMsg %d: %v", i, err)
		}
	}
	for i := range 3 {
		var ev Event
		if err := ReadMsg(&buf, &ev); err != nil {
			t.Fatalf("ReadMsg %d: %v", i, err)
		}
		if ev.Status != 200+i {
			t.Errorf("frame %d: got status %d, want %d", i, ev.Status, 200+i)
		}
	}
}

func TestReadMsgRejectsOversizedFrame(t *testing.T) {
	// A hostile peer must not be able to make the server allocate without bound.
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], maxMsg+1)
	err := ReadMsg(bytes.NewReader(hdr[:]), &Hello{})
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("got %v, want a 'too large' error", err)
	}
}

func TestReadMsgRejectsEmptyFrame(t *testing.T) {
	var hdr [4]byte // length 0
	if err := ReadMsg(bytes.NewReader(hdr[:]), &Hello{}); err == nil {
		t.Fatal("expected an error for a zero-length frame")
	}
}

func TestReadMsgOnTruncatedStream(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMsg(&buf, Hello{V: 1, Token: "zk_a_b"}); err != nil {
		t.Fatal(err)
	}
	truncated := buf.Bytes()[:buf.Len()-3]
	if err := ReadMsg(bytes.NewReader(truncated), &Hello{}); err == nil {
		t.Fatal("expected an error on a truncated frame")
	}
}
