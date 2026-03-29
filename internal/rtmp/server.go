package rtmp

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"

	goamf0 "github.com/yutopp/go-amf0"
	gortmp "github.com/yutopp/go-rtmp"
	"github.com/yutopp/go-rtmp/message"

	"github.com/titagaki/peercast-mm/internal/channel"
)

// Server listens for RTMP push connections from an encoder.
type Server struct {
	srv  *gortmp.Server
	port int
}

// NewServer creates an RTMPServer that feeds data into the given channel.
func NewServer(ch *channel.Channel, port int) *Server {
	s := &Server{port: port}
	s.srv = gortmp.NewServer(&gortmp.ServerConfig{
		OnConnect: func(conn net.Conn) (io.ReadWriteCloser, *gortmp.ConnConfig) {
			slog.Info("rtmp: encoder connected", "remote", conn.RemoteAddr())
			h := newHandler(ch, conn.RemoteAddr().String())
			return conn, &gortmp.ConnConfig{Handler: h}
		},
	})
	return s
}

// ListenAndServe starts listening on the configured RTMP port.
func (s *Server) ListenAndServe() error {
	l, err := net.Listen("tcp", fmt.Sprintf(":%d", s.port))
	if err != nil {
		return fmt.Errorf("rtmp: listen: %w", err)
	}
	return s.srv.Serve(l)
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
	ch         *channel.Channel
	remoteAddr string

	// Accumulated sequence headers and metadata.
	metaTag    []byte // onMetaData FLV tag (timestamp zeroed)
	avcTag     []byte // AVC sequence header FLV tag (timestamp zeroed)
	aacTag     []byte // AAC sequence header FLV tag (timestamp zeroed)
	headerSent bool   // true once the first complete header has been built

	streamPos uint32 // running byte position counter
}

func newHandler(ch *channel.Channel, remoteAddr string) *handler {
	return &handler{ch: ch, remoteAddr: remoteAddr}
}

// OnSetDataFrame handles the onMetaData AMF0 script tag.
func (h *handler) OnSetDataFrame(timestamp uint32, data *message.NetStreamSetDataFrame) error {
	// data.Payload is the raw AMF0 bytes: "onMetaData" string + object.
	h.metaTag = makeFLVTag(18, 0, data.Payload)
	h.maybeUpdateInfo(data)
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
	cont := body[0] != 0x17
	h.writeData(tag, cont)
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
	h.writeData(tag, true)
	return nil
}

func (h *handler) OnClose() {
	slog.Info("rtmp: encoder disconnected", "remote", h.remoteAddr)
}

// rebuildHeader assembles the FLV head packet from accumulated sequence headers
// and calls SetHeader on the channel if all required parts are present.
func (h *handler) rebuildHeader() {
	if h.avcTag == nil && h.aacTag == nil {
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

	h.ch.SetHeader(head)
	if !h.headerSent {
		h.headerSent = true
		slog.Info("rtmp: stream started", "remote", h.remoteAddr)
	}
}

func (h *handler) writeData(tag []byte, cont bool) {
	pos := h.streamPos
	h.streamPos += uint32(len(tag))
	h.ch.Write(tag, pos, cont)
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

// maybeUpdateInfo extracts bitrate from onMetaData payload and updates the channel.
// data.Payload contains the raw AMF0 bytes: "onMetaData" string + object.
func (h *handler) maybeUpdateInfo(data *message.NetStreamSetDataFrame) {
	dec := goamf0.NewDecoder(bytes.NewReader(data.Payload))

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

	info := h.ch.Info()

	if v, ok := m["videodatarate"]; ok {
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

	h.ch.SetInfo(info)
}
