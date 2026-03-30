package servent

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"

	"github.com/titagaki/peercast-pcp/pcp"

	"github.com/titagaki/peercast-mm/internal/channel"
	"github.com/titagaki/peercast-mm/internal/version"
)

// ChannelStore provides channel lookup by ID.
type ChannelStore interface {
	GetByID(channelID pcp.GnuID) (*channel.Channel, bool)
}

// Listener accepts incoming connections on the PeerCast port and dispatches them
// to the appropriate output stream handler.
type Listener struct {
	sessionID    pcp.GnuID
	mgr          ChannelStore
	port         int
	maxRelays    int // 0 = unlimited
	maxListeners int // 0 = unlimited
	listener     net.Listener
	nextConnID   atomic.Int64
	apiHandler   http.Handler // JSON-RPC handler for POST /api/; may be nil
}

// NewListener creates a new Listener.
// maxRelays and maxListeners set the per-channel connection limits (0 = unlimited).
func NewListener(sessionID pcp.GnuID, mgr ChannelStore, port, maxRelays, maxListeners int) *Listener {
	return &Listener{
		sessionID:    sessionID,
		mgr:          mgr,
		port:         port,
		maxRelays:    maxRelays,
		maxListeners: maxListeners,
	}
}

// SetAPIHandler sets the HTTP handler used for POST /api/ requests.
func (l *Listener) SetAPIHandler(h http.Handler) {
	l.apiHandler = h
}

// Listen binds to the configured PeerCast port. It must be called before Serve.
func (l *Listener) Listen() error {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", l.port))
	if err != nil {
		return fmt.Errorf("servent: listen: %w", err)
	}
	l.listener = ln
	return nil
}

// Serve accepts incoming connections. Listen must be called first.
func (l *Listener) Serve() error {
	ln := l.listener
	defer ln.Close()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go l.handle(conn)
	}
}

// ListenAndServe is a convenience wrapper around Listen + Serve.
func (l *Listener) ListenAndServe() error {
	if err := l.Listen(); err != nil {
		return err
	}
	return l.Serve()
}

// Close shuts down the listener.
func (l *Listener) Close() {
	if l.listener != nil {
		l.listener.Close()
	}
}

func (l *Listener) handle(conn net.Conn) {
	cc := newCountingConn(conn)
	br := bufio.NewReader(conn)

	// Peek enough bytes to identify the protocol and extract a 32-hex channel ID.
	// "GET /channel/<32-hex>" = 13 + 32 = 45 chars; 64 bytes is sufficient.
	peek, err := br.Peek(64)
	if err != nil && len(peek) < 4 {
		conn.Close()
		return
	}

	switch {
	case startsWith(peek, "GET /channel/"):
		l.handlePCPRelay(cc, br, peek)
	case startsWith(peek, "GET /stream/"):
		l.handleHTTPStream(cc, br, peek)
	case startsWith(peek, "pcp\n"):
		slog.Debug("servent: ping", "remote", conn.RemoteAddr())
		handlePing(conn, br, l.sessionID)
	case startsWith(peek, "POST /api"):
		if l.apiHandler != nil {
			l.handleAPIRequest(conn, br)
		} else {
			conn.Close()
		}

	default:
		slog.Warn("servent: unknown protocol", "remote", conn.RemoteAddr())
		conn.Close()
	}
}

func (l *Listener) handlePCPRelay(cc *countingConn, br *bufio.Reader, peek []byte) {
	channelID, ok := parseChannelIDFromPath(peek, "/channel/")
	if !ok {
		slog.Warn("pcp: bad channel path", "remote", cc.RemoteAddr())
		cc.Close()
		return
	}
	ch, ok := l.mgr.GetByID(channelID)
	if !ok {
		slog.Info("pcp: channel not found", "remote", cc.RemoteAddr(), "id", hex.EncodeToString(channelID[:]))
		cc.Close()
		return
	}
	id := int(l.nextConnID.Add(1))
	h := newPCPOutputStream(cc, br, l.sessionID, ch, id)
	if !ch.TryAddOutput(h, l.maxRelays, l.maxListeners) {
		slog.Info("pcp: relay rejected (relay full)", "remote", cc.RemoteAddr())
		cc.Close()
		return
	}
	slog.Info("pcp: relay connected", "remote", cc.RemoteAddr(), "id", id)
	h.run()
	ch.RemoveOutput(h)
}

func (l *Listener) handleHTTPStream(cc *countingConn, br *bufio.Reader, peek []byte) {
	channelID, ok := parseChannelIDFromPath(peek, "/stream/")
	if !ok {
		slog.Warn("http: bad stream path", "remote", cc.RemoteAddr())
		cc.Close()
		return
	}
	ch, ok := l.mgr.GetByID(channelID)
	if !ok {
		slog.Info("http: channel not found", "remote", cc.RemoteAddr(), "id", hex.EncodeToString(channelID[:]))
		cc.Close()
		return
	}
	id := int(l.nextConnID.Add(1))
	h := newHTTPOutputStream(cc, br, ch, id)
	if !ch.TryAddOutput(h, l.maxRelays, l.maxListeners) {
		slog.Info("http: viewer rejected (direct full)", "remote", cc.RemoteAddr())
		cc.Close()
		return
	}
	slog.Info("http: viewer connected", "remote", cc.RemoteAddr(), "id", id)
	h.run()
	ch.RemoveOutput(h)
}

func startsWith(data []byte, prefix string) bool {
	if len(data) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		if data[i] != prefix[i] {
			return false
		}
	}
	return true
}

// parseChannelIDFromPath extracts a 32-hex-char channel ID from the peeked
// bytes. pathPrefix is the URL path segment before the ID (e.g. "/channel/").
// The peek slice starts with "GET ".
func parseChannelIDFromPath(peek []byte, pathPrefix string) (pcp.GnuID, bool) {
	s := string(peek)
	idx := indexString(s, pathPrefix)
	if idx < 0 {
		return pcp.GnuID{}, false
	}
	start := idx + len(pathPrefix)
	if start+32 > len(s) {
		return pcp.GnuID{}, false
	}
	hexStr := s[start : start+32]
	b, err := hex.DecodeString(hexStr)
	if err != nil || len(b) != 16 {
		return pcp.GnuID{}, false
	}
	var id pcp.GnuID
	copy(id[:], b)
	return id, true
}

func indexString(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// handleAPIRequest handles a JSON-RPC request forwarded from the listener.
func (l *Listener) handleAPIRequest(conn net.Conn, br *bufio.Reader) {
	defer conn.Close()
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	defer req.Body.Close()
	rw := newAPIResponseWriter(conn)
	l.apiHandler.ServeHTTP(rw, req)
	rw.flush()
}

// apiResponseWriter is a minimal http.ResponseWriter that buffers the response
// body and writes it to the underlying connection as a plain HTTP/1.0 response.
type apiResponseWriter struct {
	conn   net.Conn
	header http.Header
	status int
	body   bytes.Buffer
}

func newAPIResponseWriter(conn net.Conn) *apiResponseWriter {
	return &apiResponseWriter{conn: conn, header: make(http.Header), status: http.StatusOK}
}

func (w *apiResponseWriter) Header() http.Header          { return w.header }
func (w *apiResponseWriter) WriteHeader(code int)         { w.status = code }
func (w *apiResponseWriter) Write(b []byte) (int, error)  { return w.body.Write(b) }

func (w *apiResponseWriter) flush() {
	body := w.body.Bytes()
	resp := &http.Response{
		StatusCode:    w.status,
		ProtoMajor:    1,
		ProtoMinor:    0,
		Header:        w.header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
	}
	resp.Write(w.conn)
}

// handlePing handles a firewall reachability check connection from the YP.
// The YP sends "pcp\n" + helo; we reply with oleh (sid) + quit.
func handlePing(conn net.Conn, br *bufio.Reader, sessionID pcp.GnuID) {
	defer conn.Close()

	// Read "pcp\n" magic: 4-byte tag + 4-byte length + 4-byte version payload.
	magic := make([]byte, 12)
	if _, err := io.ReadFull(br, magic); err != nil {
		return
	}

	heloAtom, err := pcp.ReadAtom(br)
	if err != nil || heloAtom.Tag != pcp.PCPHelo {
		return
	}

	oleh := pcp.NewParentAtom(pcp.PCPOleh,
		pcp.NewStringAtom(pcp.PCPHeloAgent, version.AgentName),
		pcp.NewIDAtom(pcp.PCPHeloSessionID, sessionID),
		pcp.NewIntAtom(pcp.PCPHeloVersion, version.PCPVersion),
	)
	if err := oleh.Write(conn); err != nil {
		return
	}

	pcp.NewIntAtom(pcp.PCPQuit, pcp.PCPErrorQuit+pcp.PCPErrorShutdown).Write(conn)
}
