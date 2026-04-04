package servent

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/titagaki/peercast-mi/internal/channel"
)

const (
	directWriteTimeout = 60 * time.Second
)

// HTTPOutputStream sends raw FLV data to a media player over HTTP.
type HTTPOutputStream struct {
	outputBase
	br *bufio.Reader
	ch *channel.Channel
}

func newHTTPOutputStream(conn *countingConn, br *bufio.Reader, ch *channel.Channel, id int) *HTTPOutputStream {
	o := &HTTPOutputStream{
		outputBase: newOutputBase(conn, id),
		br:         br,
		ch:         ch,
	}
	o.onClose = func() { conn.Close() }
	return o
}

// Type implements channel.OutputStream.
func (o *HTTPOutputStream) Type() channel.OutputStreamType { return channel.OutputStreamHTTP }

func (o *HTTPOutputStream) run() {
	defer slog.Info("http: viewer disconnected", "remote", o.remoteAddr, "id", o.id)
	defer o.conn.Close()

	req, err := http.ReadRequest(o.br)
	if err != nil {
		slog.Debug("http: read request error", "remote", o.remoteAddr, "id", o.id, "err", err)
		return
	}
	slog.Debug("http: request received", "remote", o.remoteAddr, "id", o.id, "path", req.URL.Path)
	_ = req.Body.Close()

	// Monitor the read side so we detect viewer disconnect even when no
	// new stream data is arriving (and thus no writes are attempted).
	go func() {
		buf := make([]byte, 1)
		for {
			if _, err := o.conn.Read(buf); err != nil {
				o.Close()
				return
			}
		}
	}()

	// Send HTTP response headers immediately so the player does not time out
	// waiting for a response while the relay chain is being established.
	info := o.ch.Info()
	mimeType := info.MIMEType
	if mimeType == "" {
		mimeType = "video/x-flv"
	}
	var sb strings.Builder
	sb.WriteString("HTTP/1.0 200 OK\r\n")
	sb.WriteString(fmt.Sprintf("Content-Type: %s\r\n", sanitizeHeaderValue(mimeType)))
	sb.WriteString(fmt.Sprintf("icy-name: %s\r\n", sanitizeHeaderValue(info.Name)))
	sb.WriteString(fmt.Sprintf("icy-genre: %s\r\n", sanitizeHeaderValue(info.Genre)))
	sb.WriteString(fmt.Sprintf("icy-url: %s\r\n", sanitizeHeaderValue(info.URL)))
	sb.WriteString(fmt.Sprintf("icy-bitrate: %d\r\n", info.Bitrate))
	sb.WriteString("\r\n")
	if _, err := io.WriteString(o.conn, sb.String()); err != nil {
		return
	}

	if !o.ch.HasData() {
		// Wait for the first data packet (e.g. while relay is being established).
		select {
		case <-o.ch.Signal():
			// data arrived
		case <-o.closeCh:
			return
		case <-time.After(30 * time.Second):
			return
		}
	}

	// Send stream header (FLV header / codec config).
	header, _ := o.ch.Header()
	if len(header) > 0 {
		o.conn.SetWriteDeadline(time.Now().Add(directWriteTimeout))
		if _, err := o.conn.Write(header); err != nil {
			return
		}
	}

	// Stream data starting from the next keyframe.
	var pos uint32
	waitingForKeyframe := true

	for {
		sigCh := o.ch.Signal()
		packets := o.ch.Since(pos)

		if len(packets) == 0 {
			select {
			case <-o.closeCh:
				return
			case <-sigCh:
				continue
			}
		}

		for _, pkt := range packets {
			if waitingForKeyframe && pkt.ContFlags != 0 {
				pos = pkt.Pos + uint32(len(pkt.Data))
				continue
			}
			waitingForKeyframe = false

			o.conn.SetWriteDeadline(time.Now().Add(directWriteTimeout))
			if _, err := o.conn.Write(pkt.Data); err != nil {
				return
			}
			pos = pkt.Pos + uint32(len(pkt.Data))
		}
	}
}

// sanitizeHeaderValue removes CR and LF characters from s to prevent
// HTTP header injection.
func sanitizeHeaderValue(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}
