package servent

import (
	"bufio"
	"bytes"
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

// Listener accepts incoming connections on the PeerCast port and dispatches them
// to the appropriate output stream handler.
type Listener struct {
	sessionID  pcp.GnuID
	ch         *channel.Channel
	port       int
	listener   net.Listener
	nextConnID atomic.Int64
	apiHandler http.Handler // JSON-RPC handler for POST /api/; may be nil
}

// NewListener creates a new Listener.
func NewListener(sessionID pcp.GnuID, ch *channel.Channel, port int) *Listener {
	return &Listener{
		sessionID: sessionID,
		ch:        ch,
		port:      port,
	}
}

// SetAPIHandler sets the HTTP handler used for POST /api/ requests.
func (l *Listener) SetAPIHandler(h http.Handler) {
	l.apiHandler = h
}

// ListenAndServe starts listening on the configured PeerCast port.
func (l *Listener) ListenAndServe() error {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", l.port))
	if err != nil {
		return fmt.Errorf("servent: listen: %w", err)
	}
	l.listener = ln
	defer ln.Close()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go l.handle(conn)
	}
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

	// Peek up to 16 bytes to identify the protocol.
	peek, err := br.Peek(16)
	if err != nil && len(peek) < 4 {
		conn.Close()
		return
	}

	switch {
	case startsWith(peek, "GET /channel/"):
		id := int(l.nextConnID.Add(1))
		slog.Info("pcp: relay connected", "remote", conn.RemoteAddr(), "id", id)
		h := newPCPOutputStream(cc, br, l.sessionID, l.ch, id)
		l.ch.AddOutput(h)
		h.run()
		l.ch.RemoveOutput(h)

	case startsWith(peek, "GET /stream/"):
		id := int(l.nextConnID.Add(1))
		slog.Info("http: viewer connected", "remote", conn.RemoteAddr(), "id", id)
		h := newHTTPOutputStream(cc, br, l.ch, id)
		l.ch.AddOutput(h)
		h.run()
		l.ch.RemoveOutput(h)

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
