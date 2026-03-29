package output

import (
	"bufio"
	"fmt"
	"log"
	"net"

	"github.com/titagaki/peercast-pcp/pcp"

	"github.com/titagaki/peercast-mm/internal/channel"
)

const defaultPCPPort = 7144

// Listener accepts incoming connections on the PCP port and dispatches them
// to the appropriate output stream handler.
type Listener struct {
	sessionID pcp.GnuID
	ch        *channel.Channel
	listener  net.Listener
}

// NewListener creates a new OutputListener.
func NewListener(sessionID pcp.GnuID, ch *channel.Channel) *Listener {
	return &Listener{
		sessionID: sessionID,
		ch:        ch,
	}
}

// ListenAndServe starts listening on port 7144.
func (l *Listener) ListenAndServe() error {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", defaultPCPPort))
	if err != nil {
		return fmt.Errorf("output: listen: %w", err)
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
	br := bufio.NewReader(conn)

	// Peek up to 16 bytes to identify the protocol.
	peek, err := br.Peek(16)
	if err != nil && len(peek) < 4 {
		conn.Close()
		return
	}

	switch {
	case startsWith(peek, "GET /channel/"):
		log.Printf("output: PCP relay connection from %s", conn.RemoteAddr())
		h := newPCPOutputStream(conn, br, l.sessionID, l.ch)
		l.ch.AddOutput(h)
		h.run()
		l.ch.RemoveOutput(h)

	case startsWith(peek, "GET /stream/"):
		log.Printf("output: HTTP direct connection from %s", conn.RemoteAddr())
		h := newHTTPOutputStream(conn, br, l.ch)
		l.ch.AddOutput(h)
		h.run()
		l.ch.RemoveOutput(h)

	case startsWith(peek, "pcp\n"):
		// Future: ping handler
		conn.Close()

	default:
		log.Printf("output: unknown protocol from %s, closing", conn.RemoteAddr())
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
