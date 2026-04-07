package rtmp

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"regexp"
	"strconv"

	goamf0 "github.com/yutopp/go-amf0"
	gortmp "github.com/yutopp/go-rtmp"
	"github.com/yutopp/go-rtmp/message"

	"github.com/titagaki/peercast-pcp/pcp"

	"github.com/titagaki/peercast-mi/internal/channel"
)

// ChannelManager is the subset of channel.Manager that the RTMP server needs.
type ChannelManager interface {
	IsIssuedKey(key string) bool
	GetByStreamKey(key string) (*channel.Channel, bool)
	Stop(channelID pcp.GnuID) bool
}

// Server listens for RTMP push connections from an encoder.
type Server struct {
	srv      *gortmp.Server
	port     int
	listener net.Listener
}

// NewServer creates an RTMP server that feeds incoming streams into channels
// managed by mgr. Each encoder connection must publish with a stream key that
// was previously issued via mgr.IssueStreamKey(); otherwise the connection is
// rejected in OnPublish.
func NewServer(mgr ChannelManager, port int) *Server {
	s := &Server{port: port}
	s.srv = gortmp.NewServer(&gortmp.ServerConfig{
		OnConnect: func(conn net.Conn) (io.ReadWriteCloser, *gortmp.ConnConfig) {
			slog.Info("rtmp: encoder connected", "remote", conn.RemoteAddr())
			h := newHandler(mgr, conn.RemoteAddr().String())
			return conn, &gortmp.ConnConfig{Handler: h}
		},
	})
	return s
}

// Listen binds to the configured RTMP port. It must be called before Serve.
func (s *Server) Listen() error {
	l, err := net.Listen("tcp", fmt.Sprintf(":%d", s.port))
	if err != nil {
		return fmt.Errorf("rtmp: listen: %w", err)
	}
	s.listener = l
	return nil
}

// Serve accepts RTMP connections. Listen must be called first.
func (s *Server) Serve() error {
	return s.srv.Serve(s.listener)
}

// ListenAndServe is a convenience wrapper around Listen + Serve.
func (s *Server) ListenAndServe() error {
	if err := s.Listen(); err != nil {
		return err
	}
	return s.Serve()
}

// Close shuts down the RTMP server.
func (s *Server) Close() {
	s.srv.Close()
}

// ---------------------------------------------------------------------------
// RTMP handler
// ---------------------------------------------------------------------------

type handler struct {
	gortmp.DefaultHandler
	mgr        ChannelManager
	remoteAddr string
	streamKey  string // set in OnPublish; empty until then

	// Accumulated sequence headers and metadata.
	metaTag    []byte // onMetaData FLV tag (timestamp zeroed)
	avcTag     []byte // AVC sequence header FLV tag (timestamp zeroed)
	aacTag     []byte // AAC sequence header FLV tag (timestamp zeroed)
	headerSent bool   // true once the first complete header has been built

	metaPayload []byte // raw AMF0 payload from onMetaData (for deferred info update)
	infoApplied bool   // true once metadata info has been applied to the channel

	streamPos uint32 // running byte position counter
}

func newHandler(mgr ChannelManager, remoteAddr string) *handler {
	return &handler{mgr: mgr, remoteAddr: remoteAddr}
}

// ch returns the active channel for this connection's stream key, or nil if
// OnPublish has not been called yet or broadcastChannel has not been called.
// Data received before broadcastChannel is called is silently dropped.
func (h *handler) ch() *channel.Channel {
	if h.streamKey == "" {
		return nil
	}
	ch, _ := h.mgr.GetByStreamKey(h.streamKey)
	return ch
}

// OnPublish validates the publishing stream key against the manager's issued
// keys. Returns an error (causing go-rtmp to reject the stream) if the key is
// unknown.
func (h *handler) OnPublish(_ *gortmp.StreamContext, _ uint32, cmd *message.NetStreamPublish) error {
	key := cmd.PublishingName
	if !h.mgr.IsIssuedKey(key) {
		slog.Warn("rtmp: rejected unknown stream key", "remote", h.remoteAddr, "key", key)
		return fmt.Errorf("rtmp: stream key %q not issued", key)
	}
	h.streamKey = key
	slog.Info("rtmp: stream key accepted", "remote", h.remoteAddr, "key", key)
	return nil
}

// OnSetDataFrame handles the onMetaData AMF0 script tag.
func (h *handler) OnSetDataFrame(timestamp uint32, data *message.NetStreamSetDataFrame) error {
	// data.Payload is the raw AMF0 bytes: "onMetaData" string + object.
	h.metaTag = makeFLVTag(18, 0, data.Payload)
	h.metaPayload = append([]byte(nil), data.Payload...)
	h.applyMetaInfo()
	h.rebuildHeader()
	return nil
}

// OnVideo handles video frames.
func (h *handler) OnVideo(timestamp uint32, payload io.Reader) error {
	body, err := io.ReadAll(payload)
	if err != nil {
		return err
	}
	if len(body) < 2 {
		return nil
	}

	tag := makeFLVTag(9, timestamp, body)

	if body[0] == 0x17 && body[1] == 0x00 {
		// AVC sequence header
		h.avcTag = makeFLVTag(9, 0, body)
		h.rebuildHeader()
		return nil
	}

	// keyframe (0x17) or inter (0x27)
	var contFlags byte
	if body[0] != 0x17 {
		contFlags = 0x02 // InterFrame
	}
	h.writeData(tag, contFlags)
	return nil
}

// OnAudio handles audio frames.
func (h *handler) OnAudio(timestamp uint32, payload io.Reader) error {
	body, err := io.ReadAll(payload)
	if err != nil {
		return err
	}
	if len(body) < 2 {
		return nil
	}

	if body[0] == 0xAF && body[1] == 0x00 {
		// AAC sequence header
		h.aacTag = makeFLVTag(8, 0, body)
		h.rebuildHeader()
		return nil
	}

	tag := makeFLVTag(8, timestamp, body)
	h.writeData(tag, 0x04) // AudioFrame
	return nil
}

func (h *handler) OnClose() {
	slog.Info("rtmp: encoder disconnected", "remote", h.remoteAddr)
	if h.streamKey == "" {
		return
	}
	ch, ok := h.mgr.GetByStreamKey(h.streamKey)
	if !ok {
		return
	}
	slog.Info("rtmp: stopping channel on encoder disconnect", "key", h.streamKey, "channel_id", ch.ID)
	h.mgr.Stop(ch.ID)
}

// rebuildHeader assembles the FLV head packet from accumulated sequence headers
// and calls SetHeader on the channel. Silently dropped if no active channel
// exists for the stream key yet (broadcastChannel not yet called).
func (h *handler) rebuildHeader() {
	if h.avcTag == nil && h.aacTag == nil {
		return
	}
	// Retry deferred metadata info update (covers the case where
	// OnSetDataFrame arrived before broadcastChannel created the channel).
	h.applyMetaInfo()
	ch := h.ch()
	if ch == nil {
		return
	}

	var head []byte

	// FLV file header: "FLV" + version(1) + flags(0x05) + dataOffset(9) + backPointer(0)
	head = append(head, []byte("FLV")...)
	head = append(head, 0x01)       // version
	head = append(head, 0x05)       // flags: hasVideo | hasAudio
	head = append(head, 0, 0, 0, 9) // dataOffset = 9
	head = append(head, 0, 0, 0, 0) // PreviousTagSize0 = 0

	if h.metaTag != nil {
		head = appendFLVTagWithBackPointer(head, h.metaTag)
	}
	if h.avcTag != nil {
		head = appendFLVTagWithBackPointer(head, h.avcTag)
	}
	if h.aacTag != nil {
		head = appendFLVTagWithBackPointer(head, h.aacTag)
	}

	slog.Debug("rtmp: rebuildHeader",
		"remote", h.remoteAddr,
		"key", h.streamKey,
		"headerSize", len(head),
		"hasMeta", h.metaTag != nil,
		"metaSize", len(h.metaTag),
		"hasAVC", h.avcTag != nil,
		"avcSize", len(h.avcTag),
		"hasAAC", h.aacTag != nil,
		"aacSize", len(h.aacTag),
	)

	ch.SetHeader(head, 0)
	if !h.headerSent {
		h.headerSent = true
		slog.Info("rtmp: stream started", "remote", h.remoteAddr, "key", h.streamKey)
	}
}

func (h *handler) writeData(tag []byte, contFlags byte) {
	h.applyMetaInfo()
	ch := h.ch()
	if ch == nil {
		return
	}
	// Append PreviousTagSize (4 bytes big-endian) so that concatenated data
	// packets form a valid FLV byte stream.
	data := appendFLVTagWithBackPointer(nil, tag)
	pos := h.streamPos
	h.streamPos += uint32(len(data))
	ch.Write(data, pos, contFlags)
}

// ---------------------------------------------------------------------------
// FLV tag helpers
// ---------------------------------------------------------------------------

// makeFLVTag builds an 11-byte FLV tag header + body. Back pointer is NOT included here;
// it is appended separately when composing the head packet.
func makeFLVTag(tagType byte, timestamp uint32, body []byte) []byte {
	dataSize := len(body)
	tag := make([]byte, 11+dataSize)
	tag[0] = tagType
	// DataSize: 3 bytes big-endian
	tag[1] = byte(dataSize >> 16)
	tag[2] = byte(dataSize >> 8)
	tag[3] = byte(dataSize)
	// Timestamp lower 24 bits
	tag[4] = byte(timestamp >> 16)
	tag[5] = byte(timestamp >> 8)
	tag[6] = byte(timestamp)
	// TimestampExt: upper 8 bits
	tag[7] = byte(timestamp >> 24)
	// StreamID: 3 bytes, always 0
	tag[8] = 0
	tag[9] = 0
	tag[10] = 0
	copy(tag[11:], body)
	return tag
}

// appendFLVTagWithBackPointer appends an FLV tag and its 4-byte back pointer to buf.
func appendFLVTagWithBackPointer(buf, tag []byte) []byte {
	buf = append(buf, tag...)
	size := uint32(len(tag))
	bp := make([]byte, 4)
	binary.BigEndian.PutUint32(bp, size)
	buf = append(buf, bp...)
	return buf
}

// ---------------------------------------------------------------------------
// AMF0 helpers
// ---------------------------------------------------------------------------

// applyMetaInfo extracts bitrate from the saved onMetaData payload and updates
// the channel. It is called from OnSetDataFrame and retried from rebuildHeader
// so that metadata arriving before broadcastChannel is not lost.
func (h *handler) applyMetaInfo() {
	if h.infoApplied || len(h.metaPayload) == 0 {
		return
	}
	ch := h.ch()
	if ch == nil {
		return
	}
	dec := goamf0.NewDecoder(bytes.NewReader(h.metaPayload))

	// Skip the "onMetaData" string.
	var name string
	if err := dec.Decode(&name); err != nil {
		return
	}

	// Decode the metadata object.
	var obj interface{}
	if err := dec.Decode(&obj); err != nil {
		return
	}

	m, ok := obj.(goamf0.ECMAArray)
	if !ok {
		if mv, ok2 := obj.(map[string]interface{}); ok2 {
			m = goamf0.ECMAArray(mv)
		} else {
			return
		}
	}

	info := ch.Info()

	// Extract video bitrate: prefer maxBitrate (OBS sets this instead of
	// videodatarate in some configurations), fall back to videodatarate.
	// PeerCastStation uses the same priority order.
	if br, ok := parseMaxBitrate(m["maxBitrate"]); ok {
		info.Bitrate = uint32(br)
	} else if v, ok := m["videodatarate"]; ok {
		if f, ok := v.(float64); ok {
			info.Bitrate = uint32(f)
		}
	}
	if v, ok := m["audiodatarate"]; ok {
		if f, ok := v.(float64); ok {
			info.Bitrate += uint32(f)
		}
	}
	if info.Type == "" {
		info.Type = "FLV"
		info.MIMEType = "video/x-flv"
		info.Ext = ".flv"
	}

	ch.SetInfo(info)
	h.infoApplied = true
}

var reKbpsSuffix = regexp.MustCompile(`(\d+)k`)

// parseMaxBitrate parses the maxBitrate AMF value. OBS may set this as a
// number or a string like "5000k". Returns the bitrate in kbps and true
// if a valid value was found.
func parseMaxBitrate(v interface{}) (float64, bool) {
	if v == nil {
		return 0, false
	}
	switch val := v.(type) {
	case float64:
		if val > 0 {
			return val, true
		}
	case string:
		s := reKbpsSuffix.ReplaceAllString(val, "$1")
		if f, err := strconv.ParseFloat(s, 64); err == nil && f > 0 {
			return f, true
		}
	}
	return 0, false
}
