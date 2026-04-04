package relay

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/titagaki/peercast-pcp/pcp"

	"github.com/titagaki/peercast-mi/internal/channel"
	"github.com/titagaki/peercast-mi/internal/pcputil"
	"github.com/titagaki/peercast-mi/internal/version"
)

const (
	dialTimeout  = 10 * time.Second
	readTimeout  = 60 * time.Second
	retryInitial = 5 * time.Second
	retryMax     = 120 * time.Second
)

var (
	pktTypeHead = pcp.NewID4("head")
	pktTypeData = pcp.NewID4("data")
)

const (
	bcstInterval     = 30 * time.Second
	bcstWriteTimeout = 10 * time.Second
)

// stopReason classifies why a connection ended.
type stopReason int

const (
	stopReasonError         stopReason = iota // I/O or protocol error (no data received)
	stopReasonErrorAfterData                  // I/O error after successfully receiving data
	stopReasonUnavailable                     // quit code 1003
	stopReasonOffAir                          // quit with other code
)

// Client connects to an upstream PeerCast node and writes the received stream
// into a local channel, reconnecting on failure.
type Client struct {
	trackerAddr  string
	channelID    pcp.GnuID
	sessionID    pcp.GnuID
	listenPort   uint16
	ch           *channel.Channel

	sourceNodes  *SourceNodeList
	ignoredNodes *IgnoredNodeCollection

	globalIP atomic.Uint32 // learned from YP oleh

	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once
}

// SetGlobalIP updates the global IP address reported in BCST HOST atoms.
func (c *Client) SetGlobalIP(ip uint32) {
	c.globalIP.Store(ip)
}

// New creates a new relay Client.
func New(trackerAddr string, channelID, sessionID pcp.GnuID, listenPort uint16, ch *channel.Channel) *Client {
	return &Client{
		trackerAddr:  trackerAddr,
		channelID:    channelID,
		sessionID:    sessionID,
		listenPort:   listenPort,
		ch:           ch,
		sourceNodes:  NewSourceNodeList(),
		ignoredNodes: NewIgnoredNodeCollection(),
		stopCh:       make(chan struct{}),
		doneCh:       make(chan struct{}),
	}
}

// Run connects to the upstream node and runs the receive loop, reconnecting on
// failure with exponential backoff. It blocks until Stop is called.
//
// Host selection follows PeerCastStation's algorithm: source nodes learned from
// PCP HOST atoms are scored and the best candidate is chosen. Hosts that return
// UNAVAILABLE (code 1003) or fail are temporarily ignored for 3 minutes.
func (c *Client) Run() {
	defer close(c.doneCh)
	delay := retryInitial

	for {
		targetAddr := selectSourceHost(c.sourceNodes.List(), c.ignoredNodes, c.trackerAddr)
		if targetAddr == "" {
			// All hosts exhausted — wait with backoff.
			select {
			case <-c.stopCh:
				return
			case <-time.After(delay):
			}
			delay *= 2
			if delay > retryMax {
				delay = retryMax
			}
			continue
		}

		reason, err := c.connectTo(targetAddr)
		if err != nil {
			slog.Error("relay: connection error", "addr", targetAddr, "err", err)
		}

		switch reason {
		case stopReasonOffAir:
			// Channel is off-air — stop retrying (matches PeerCastStation).
			slog.Info("relay: channel off-air, stopping", "addr", targetAddr)
			return
		case stopReasonUnavailable:
			// Host is full — ignore it and immediately try the next best.
			c.ignoredNodes.Add(targetAddr)
			delay = retryInitial
			select {
			case <-c.stopCh:
				return
			default:
			}
			continue
		case stopReasonErrorAfterData:
			// Connection was productive but lost — reset backoff and retry.
			delay = retryInitial
		case stopReasonError:
			// Connection or protocol error — ignore this host, try next.
			if targetAddr != c.trackerAddr {
				c.ignoredNodes.Add(targetAddr)
			}
		}

		select {
		case <-c.stopCh:
			return
		case <-time.After(delay):
		}
		delay *= 2
		if delay > retryMax {
			delay = retryMax
		}
	}
}

// Stop signals the client to shut down and waits for it to exit.
func (c *Client) Stop() {
	c.stopOnce.Do(func() { close(c.stopCh) })
	<-c.doneCh
}

func (c *Client) connectTo(addr string) (stopReason, error) {
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return stopReasonError, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	br, err := c.handshake(conn, addr)
	if err != nil {
		return stopReasonError, err
	}

	// Send periodic BCST HOST atoms upstream so intermediate nodes can
	// populate their relay tree with peercast-mi's version info.
	bcstStop := make(chan struct{})
	go c.bcstHostLoop(conn, bcstStop)
	defer close(bcstStop)

	return c.receiveLoop(conn, br)
}

// handshake performs the PCP relay handshake over an established connection:
// sends the HTTP GET + helo atom, reads the HTTP response + oleh atom.
func (c *Client) handshake(conn net.Conn, addr string) (*bufio.Reader, error) {
	chanIDHex := hex.EncodeToString(c.channelID[:])

	// 1. Send HTTP GET /channel/<id> with PCP upgrade header.
	req := fmt.Sprintf("GET /channel/%s HTTP/1.0\r\nHost: %s\r\nx-peercast-pcp: 1\r\nx-peercast-pos: 0\r\n\r\n", chanIDHex, addr)
	if _, err := io.WriteString(conn, req); err != nil {
		return nil, fmt.Errorf("write GET: %w", err)
	}

	// 2. Send helo atom.
	// Note: the pcp\n PCP_CONNECT magic is sent only for direct TCP connections
	// (not HTTP-upgraded /channel/ requests), so it is intentionally omitted here.
	helo := (&pcp.HeloPacket{
		Agent:     version.AgentName,
		SessionID: c.sessionID,
		Version:   version.PCPVersion,
		Port:      c.listenPort,
	}).BuildHeloAtom()
	if err := helo.Write(conn); err != nil {
		return nil, fmt.Errorf("write helo: %w", err)
	}

	br := bufio.NewReader(conn)

	// 3. Read HTTP response headers.
	statusCode, err := readHTTPStatus(br)
	if err != nil {
		return nil, fmt.Errorf("read HTTP response: %w", err)
	}
	if statusCode != 200 && statusCode != 503 {
		return nil, fmt.Errorf("upstream returned HTTP %d", statusCode)
	}

	// 4. Read oleh atom.
	oleh, err := pcp.ReadAtom(br)
	if err != nil {
		return nil, fmt.Errorf("read oleh: %w", err)
	}
	if oleh.Tag != pcp.PCPOleh {
		return nil, fmt.Errorf("expected oleh, got %s", oleh.Tag)
	}

	slog.Info("relay: connected", "addr", addr, "channel", chanIDHex)
	return br, nil
}

func (c *Client) receiveLoop(conn net.Conn, br *bufio.Reader) (stopReason, error) {
	receivedData := false
	for {
		select {
		case <-c.stopCh:
			return stopReasonOffAir, nil
		default:
		}

		conn.SetReadDeadline(time.Now().Add(readTimeout))
		atom, err := pcp.ReadAtom(br)
		conn.SetReadDeadline(time.Time{})
		if err != nil {
			select {
			case <-c.stopCh:
				return stopReasonOffAir, nil
			default:
			}
			reason := stopReasonError
			if receivedData {
				reason = stopReasonErrorAfterData
			}
			return reason, fmt.Errorf("read atom: %w", err)
		}

		switch atom.Tag {
		case pcp.PCPChan:
			c.handleChan(atom)
			receivedData = true
		case pcp.PCPHost:
			if node, ok := parseSourceNode(atom); ok {
				c.sourceNodes.Add(node)
			}
		case pcp.PCPQuit:
			code, _ := atom.GetInt()
			reason := stopReasonOffAir
			if code == 1003 {
				reason = stopReasonUnavailable
			}
			return reason, fmt.Errorf("quit from upstream (code %d)", code)
		}
	}
}


func (c *Client) handleChan(atom *pcp.Atom) {
	ch, err := pcp.ParseChanPacket(atom)
	if err != nil {
		return
	}
	if !ch.BroadcastID.IsEmpty() {
		c.ch.SetBroadcastID(ch.BroadcastID)
	}
	if ch.Info != nil {
		c.ch.SetInfo(channel.ChannelInfoFromPCP(*ch.Info))
	}
	if ch.Track != nil {
		c.ch.SetTrack(channel.TrackInfoFromPCP(*ch.Track))
	}
	if ch.Pkt != nil {
		c.handlePkt(ch.Pkt)
	}
}

func (c *Client) handlePkt(p *pcp.ChanPktData) {
	switch p.Type {
	case pktTypeHead:
		if len(p.Data) > 0 {
			c.ch.SetHeader(p.Data)
		}
	case pktTypeData:
		if len(p.Data) == 0 {
			return
		}
		c.ch.Write(p.Data, p.Pos, p.Continuation)
	}
}

// bcstHostLoop sends BCST HOST atoms upstream periodically so that
// intermediate nodes can build the relay tree with peercast-mi's version info.
func (c *Client) bcstHostLoop(conn net.Conn, stop <-chan struct{}) {
	uphostIP, uphostPort := connRemoteIPPort(conn)
	c.writeBcstHost(conn, uphostIP, uphostPort)
	t := time.NewTicker(bcstInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			c.writeBcstHost(conn, uphostIP, uphostPort)
		}
	}
}

func (c *Client) writeBcstHost(conn net.Conn, uphostIP uint32, uphostPort uint16) {
	atom := c.buildRelayBcstAtom(uphostIP, uphostPort)
	conn.SetWriteDeadline(time.Now().Add(bcstWriteTimeout))
	atom.Write(conn)
	conn.SetWriteDeadline(time.Time{})
}

func (c *Client) buildRelayBcstAtom(uphostIP uint32, uphostPort uint16) *pcp.Atom {
	globalIP := c.globalIP.Load()
	hostAtom := pcputil.BuildHostAtom(pcputil.HostAtomParams{
		SessionID:    c.sessionID,
		GlobalIP:     globalIP,
		ListenPort:   c.listenPort,
		ChannelID:    c.channelID,
		NumListeners: c.ch.NumListeners(),
		NumRelays:    c.ch.NumRelays(),
		Uptime:       c.ch.UptimeSeconds(),
		OldPos:       c.ch.OldestPos(),
		NewPos:       c.ch.NewestPos(),
		HasGlobalIP:  globalIP != 0,
		UphostIP:     uphostIP,
		UphostPort:   uphostPort,
		UphostHops:   1,
	})
	return pcp.NewParentAtom(pcp.PCPBcst,
		pcp.NewByteAtom(pcp.PCPBcstTTL, 7),
		pcp.NewByteAtom(pcp.PCPBcstHops, 0),
		pcp.NewIDAtom(pcp.PCPBcstFrom, c.sessionID),
		pcp.NewByteAtom(pcp.PCPBcstGroup, byte(pcp.PCPBcstGroupRoot)),
		pcp.NewIDAtom(pcp.PCPBcstChanID, c.channelID),
		pcp.NewIntAtom(pcp.PCPBcstVersion, version.PCPVersion),
		hostAtom,
	)
}

func connRemoteIPPort(conn net.Conn) (uint32, uint16) {
	tcp, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return 0, 0
	}
	v, _ := pcp.IPv4ToUint32(tcp.IP)
	return v, uint16(tcp.Port)
}

// readHTTPStatus reads the HTTP status line and all headers from br,
// returning the status code. Subsequent reads from br yield the PCP stream.
func readHTTPStatus(br *bufio.Reader) (int, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return 0, fmt.Errorf("read status line: %w", err)
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0, fmt.Errorf("invalid HTTP status line: %q", line)
	}
	code, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, fmt.Errorf("invalid status code %q: %w", fields[1], err)
	}
	// Discard headers until blank line.
	for {
		line, err = br.ReadString('\n')
		if err != nil {
			return code, fmt.Errorf("read headers: %w", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	return code, nil
}
