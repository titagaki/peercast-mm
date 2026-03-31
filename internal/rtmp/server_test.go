package rtmp

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	"github.com/yutopp/go-rtmp/message"

	"github.com/titagaki/peercast-mm/internal/channel"
	"github.com/titagaki/peercast-mm/internal/id"
)

// helpers ---------------------------------------------------------------

func setupHandler(t *testing.T) (*handler, *channel.Manager, *channel.Channel) {
	t.Helper()
	mgr := channel.NewManager(id.NewRandom())
	key := mgr.IssueStreamKey()
	info := channel.ChannelInfo{Name: "test", Genre: "test", Type: "FLV", MIMEType: "video/x-flv", Ext: ".flv"}
	ch, err := mgr.Broadcast(key, info, channel.TrackInfo{})
	if err != nil {
		t.Fatal(err)
	}
	h := newHandler(mgr, "127.0.0.1:9999")
	h.streamKey = key
	return h, mgr, ch
}

// -----------------------------------------------------------------------
// OnPublish
// -----------------------------------------------------------------------

func TestOnPublish_AcceptsIssuedKey(t *testing.T) {
	mgr := channel.NewManager(id.NewRandom())
	key := mgr.IssueStreamKey()
	h := newHandler(mgr, "127.0.0.1:9999")

	cmd := &message.NetStreamPublish{PublishingName: key}
	if err := h.OnPublish(nil, 0, cmd); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if h.streamKey != key {
		t.Fatalf("streamKey not set: got %q, want %q", h.streamKey, key)
	}
}

func TestOnPublish_RejectsUnknownKey(t *testing.T) {
	mgr := channel.NewManager(id.NewRandom())
	h := newHandler(mgr, "127.0.0.1:9999")

	cmd := &message.NetStreamPublish{PublishingName: "unknown_key"}
	if err := h.OnPublish(nil, 0, cmd); err == nil {
		t.Fatal("expected error for unknown key")
	}
	if h.streamKey != "" {
		t.Fatalf("streamKey should be empty, got %q", h.streamKey)
	}
}

// -----------------------------------------------------------------------
// makeFLVTag
// -----------------------------------------------------------------------

func TestMakeFLVTag(t *testing.T) {
	body := []byte{0x17, 0x01, 0xAA, 0xBB}
	tag := makeFLVTag(9, 1000, body)

	if len(tag) != 11+len(body) {
		t.Fatalf("tag length: got %d, want %d", len(tag), 11+len(body))
	}
	// TagType
	if tag[0] != 9 {
		t.Errorf("tag type: got %d, want 9", tag[0])
	}
	// DataSize (3 bytes big-endian)
	dataSize := int(tag[1])<<16 | int(tag[2])<<8 | int(tag[3])
	if dataSize != len(body) {
		t.Errorf("data size: got %d, want %d", dataSize, len(body))
	}
	// Timestamp lower 24 bits (big-endian) + TimestampExt (upper 8 bits)
	ts := uint32(tag[4])<<16 | uint32(tag[5])<<8 | uint32(tag[6]) | uint32(tag[7])<<24
	if ts != 1000 {
		t.Errorf("timestamp: got %d, want 1000", ts)
	}
	// StreamID always 0
	if tag[8] != 0 || tag[9] != 0 || tag[10] != 0 {
		t.Error("stream ID should be 0")
	}
	// Body
	if !bytes.Equal(tag[11:], body) {
		t.Error("body mismatch")
	}
}

func TestMakeFLVTag_LargeTimestamp(t *testing.T) {
	// Timestamp that uses the extension byte (>= 0x01000000)
	ts := uint32(0x01020304)
	tag := makeFLVTag(8, ts, []byte{0xFF})
	got := uint32(tag[4])<<16 | uint32(tag[5])<<8 | uint32(tag[6]) | uint32(tag[7])<<24
	if got != ts {
		t.Errorf("timestamp: got 0x%08X, want 0x%08X", got, ts)
	}
}

// -----------------------------------------------------------------------
// appendFLVTagWithBackPointer
// -----------------------------------------------------------------------

func TestAppendFLVTagWithBackPointer(t *testing.T) {
	tag := makeFLVTag(9, 0, []byte{0x17, 0x00})
	buf := appendFLVTagWithBackPointer(nil, tag)

	if len(buf) != len(tag)+4 {
		t.Fatalf("length: got %d, want %d", len(buf), len(tag)+4)
	}
	bp := binary.BigEndian.Uint32(buf[len(tag):])
	if bp != uint32(len(tag)) {
		t.Errorf("back pointer: got %d, want %d", bp, len(tag))
	}
}

// -----------------------------------------------------------------------
// OnVideo
// -----------------------------------------------------------------------

func TestOnVideo_AVCSequenceHeader(t *testing.T) {
	h, _, ch := setupHandler(t)

	// AVC sequence header: body[0]==0x17, body[1]==0x00
	body := []byte{0x17, 0x00, 0x01, 0x02, 0x03}
	err := h.OnVideo(0, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	// Should store as avcTag, not write data to buffer
	if h.avcTag == nil {
		t.Fatal("avcTag should be set")
	}
	// avcTag timestamp should be 0
	ts := uint32(h.avcTag[4])<<16 | uint32(h.avcTag[5])<<8 | uint32(h.avcTag[6]) | uint32(h.avcTag[7])<<24
	if ts != 0 {
		t.Error("avcTag timestamp should be 0")
	}
	// Buffer should have the header set
	header, _ := ch.Buffer.Header()
	if len(header) == 0 {
		t.Fatal("header should be set after AVC sequence header")
	}
	// Header should start with "FLV"
	if !bytes.HasPrefix(header, []byte("FLV")) {
		t.Error("header should start with FLV file header")
	}
	// No data packets should be written
	if ch.Buffer.HasData() {
		t.Error("data should not be written for sequence header")
	}
}

func TestOnVideo_Keyframe(t *testing.T) {
	h, _, ch := setupHandler(t)

	// Keyframe: body[0]==0x17, body[1]!=0x00
	body := []byte{0x17, 0x01, 0xAA}
	err := h.OnVideo(100, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	if !ch.Buffer.HasData() {
		t.Fatal("keyframe should be written to buffer")
	}
	packets := ch.Buffer.Since(0)
	if len(packets) != 1 {
		t.Fatalf("expected 1 packet, got %d", len(packets))
	}
	if packets[0].ContFlags != 0 {
		t.Error("keyframe should have ContFlags=0")
	}
}

func TestOnVideo_InterFrame(t *testing.T) {
	h, _, ch := setupHandler(t)

	// Inter frame: body[0]==0x27
	body := []byte{0x27, 0x01, 0xBB}
	err := h.OnVideo(200, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	packets := ch.Buffer.Since(0)
	if len(packets) != 1 {
		t.Fatalf("expected 1 packet, got %d", len(packets))
	}
	if packets[0].ContFlags != 0x02 {
		t.Errorf("inter frame should have ContFlags=0x02, got 0x%02x", packets[0].ContFlags)
	}
}

func TestOnVideo_ShortBody(t *testing.T) {
	h, _, ch := setupHandler(t)

	// Body < 2 bytes should be silently ignored
	err := h.OnVideo(0, bytes.NewReader([]byte{0x17}))
	if err != nil {
		t.Fatal(err)
	}
	if ch.Buffer.HasData() {
		t.Error("short body should be ignored")
	}
}

// -----------------------------------------------------------------------
// OnAudio
// -----------------------------------------------------------------------

func TestOnAudio_AACSequenceHeader(t *testing.T) {
	h, _, ch := setupHandler(t)

	// AAC sequence header: body[0]==0xAF, body[1]==0x00
	body := []byte{0xAF, 0x00, 0x12, 0x10}
	err := h.OnAudio(0, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	if h.aacTag == nil {
		t.Fatal("aacTag should be set")
	}
	header, _ := ch.Buffer.Header()
	if len(header) == 0 {
		t.Fatal("header should be set after AAC sequence header")
	}
	if ch.Buffer.HasData() {
		t.Error("no data packets should be written for sequence header")
	}
}

func TestOnAudio_DataFrame(t *testing.T) {
	h, _, ch := setupHandler(t)

	// Non-sequence-header audio: body[0]==0xAF, body[1]!=0x00
	body := []byte{0xAF, 0x01, 0xCC, 0xDD}
	err := h.OnAudio(300, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	packets := ch.Buffer.Since(0)
	if len(packets) != 1 {
		t.Fatalf("expected 1 packet, got %d", len(packets))
	}
	if packets[0].ContFlags != 0x04 {
		t.Errorf("audio data should have ContFlags=0x04, got 0x%02x", packets[0].ContFlags)
	}
}

// -----------------------------------------------------------------------
// rebuildHeader
// -----------------------------------------------------------------------

func TestRebuildHeader_FullHeader(t *testing.T) {
	h, _, ch := setupHandler(t)

	// Set all three: meta, avc, aac
	h.metaTag = makeFLVTag(18, 0, []byte("meta"))
	h.avcTag = makeFLVTag(9, 0, []byte{0x17, 0x00})
	h.aacTag = makeFLVTag(8, 0, []byte{0xAF, 0x00})
	h.rebuildHeader()

	header, _ := ch.Buffer.Header()
	if len(header) == 0 {
		t.Fatal("header should be set")
	}
	// FLV file header = 13 bytes
	// each tag has: len(tag) + 4 bytes back pointer
	expectedMin := 13 + len(h.metaTag) + 4 + len(h.avcTag) + 4 + len(h.aacTag) + 4
	if len(header) != expectedMin {
		t.Errorf("header length: got %d, want %d", len(header), expectedMin)
	}
}

func TestRebuildHeader_NoSequenceHeaders(t *testing.T) {
	h, _, ch := setupHandler(t)

	// Neither avcTag nor aacTag set — rebuildHeader should be a no-op
	h.rebuildHeader()
	header, _ := ch.Buffer.Header()
	if len(header) != 0 {
		t.Error("header should not be set without any sequence headers")
	}
}

func TestRebuildHeader_NoChannel(t *testing.T) {
	mgr := channel.NewManager(id.NewRandom())
	key := mgr.IssueStreamKey()
	h := newHandler(mgr, "127.0.0.1:9999")
	h.streamKey = key
	// Don't call Broadcast, so h.ch() returns nil.

	h.avcTag = makeFLVTag(9, 0, []byte{0x17, 0x00})
	// Should not panic
	h.rebuildHeader()
}

// -----------------------------------------------------------------------
// writeData — streamPos tracking
// -----------------------------------------------------------------------

func TestWriteData_StreamPosAdvances(t *testing.T) {
	h, _, ch := setupHandler(t)

	body1 := []byte{0x17, 0x01, 0xAA}
	tag1 := makeFLVTag(9, 100, body1)
	h.writeData(tag1, 0)

	body2 := []byte{0x27, 0x01, 0xBB}
	tag2 := makeFLVTag(9, 200, body2)
	h.writeData(tag2, 0x02)

	packets := ch.Buffer.Since(0)
	if len(packets) != 2 {
		t.Fatalf("expected 2 packets, got %d", len(packets))
	}
	if packets[0].Pos != 0 {
		t.Errorf("first packet pos: got %d, want 0", packets[0].Pos)
	}
	if packets[1].Pos != uint32(len(tag1)) {
		t.Errorf("second packet pos: got %d, want %d", packets[1].Pos, len(tag1))
	}
}

func TestWriteData_NoChannel(t *testing.T) {
	mgr := channel.NewManager(id.NewRandom())
	h := newHandler(mgr, "127.0.0.1:9999")
	// streamKey not set → ch() returns nil

	tag := makeFLVTag(9, 0, []byte{0x17, 0x01})
	// Should not panic
	h.writeData(tag, 0)
}

// -----------------------------------------------------------------------
// OnClose — lifecycle
// -----------------------------------------------------------------------

func TestOnClose_StopsChannel(t *testing.T) {
	h, mgr, ch := setupHandler(t)

	h.OnClose()

	if _, ok := mgr.GetByID(ch.ID); ok {
		t.Error("channel should be stopped after OnClose")
	}
}

func TestOnClose_NoStreamKey(t *testing.T) {
	mgr := channel.NewManager(id.NewRandom())
	h := newHandler(mgr, "127.0.0.1:9999")
	// streamKey == "" → should not panic
	h.OnClose()
}

func TestOnClose_NoBroadcast(t *testing.T) {
	mgr := channel.NewManager(id.NewRandom())
	key := mgr.IssueStreamKey()
	h := newHandler(mgr, "127.0.0.1:9999")
	h.streamKey = key
	// Key issued but no Broadcast called → ch not found → should not panic
	h.OnClose()
}

// -----------------------------------------------------------------------
// ch() helper
// -----------------------------------------------------------------------

func TestCh_BeforePublish(t *testing.T) {
	mgr := channel.NewManager(id.NewRandom())
	h := newHandler(mgr, "127.0.0.1:9999")
	if h.ch() != nil {
		t.Error("ch() should return nil before OnPublish")
	}
}

func TestCh_BeforeBroadcast(t *testing.T) {
	mgr := channel.NewManager(id.NewRandom())
	key := mgr.IssueStreamKey()
	h := newHandler(mgr, "127.0.0.1:9999")
	h.streamKey = key
	// key issued but not broadcasting
	if h.ch() != nil {
		t.Error("ch() should return nil when channel not yet broadcast")
	}
}

// -----------------------------------------------------------------------
// Data before broadcastChannel (silent drop)
// -----------------------------------------------------------------------

func TestDataBeforeBroadcast_SilentlyDropped(t *testing.T) {
	mgr := channel.NewManager(id.NewRandom())
	key := mgr.IssueStreamKey()
	h := newHandler(mgr, "127.0.0.1:9999")
	h.streamKey = key

	// Send video data without Broadcast — should not panic or error
	body := []byte{0x17, 0x01, 0xAA}
	if err := h.OnVideo(0, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	body = []byte{0xAF, 0x01, 0xBB}
	if err := h.OnAudio(0, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
}

// -----------------------------------------------------------------------
// OnVideo read error
// -----------------------------------------------------------------------

func TestOnVideo_ReadError(t *testing.T) {
	h, _, _ := setupHandler(t)
	err := h.OnVideo(0, &errReader{})
	if err == nil {
		t.Fatal("expected error from bad reader")
	}
}

func TestOnAudio_ReadError(t *testing.T) {
	h, _, _ := setupHandler(t)
	err := h.OnAudio(0, &errReader{})
	if err == nil {
		t.Fatal("expected error from bad reader")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
