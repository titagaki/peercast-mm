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

// Client connects to an upstream PeerCast node and writes the received stream
// into a local channel, reconnecting on failure.
type Client struct {
	upstreamAddr string
	channelID    pcp.GnuID
	sessionID    pcp.GnuID
	listenPort   uint16
	ch           *channel.Channel

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
func New(upstreamAddr string, channelID, sessionID pcp.GnuID, listenPort uint16, ch *channel.Channel) *Client {
	return &Client{
		upstreamAddr: upstreamAddr,
		channelID:    channelID,
		sessionID:    sessionID,
		listenPort:   listenPort,
		ch:           ch,
		stopCh:       make(chan struct{}),
		doneCh:       make(chan struct{}),
	}
}

// Run connects to the upstream node and runs the receive loop, reconnecting on
// failure with exponential backoff. It blocks until Stop is called.
// When the upstream returns 503 with relay host hints, Run tries those hosts
// immediately before falling back to the original address with backoff.
func (c *Client) Run() {
	defer close(c.doneCh)
	delay := retryInitial
	originalAddr := c.upstreamAddr
	triedAddrs := map[string]bool{}

	for {
		hitHosts, err := c.connect()
		if err != nil {
			slog.Error("relay: connection error", "addr", c.upstreamAddr, "err", err)
		}

		// If we received relay host hints (from a 503 response), try them
		// before falling back to the original address with backoff.
		nextAddr := ""
		for _, h := range hitHosts {
			if !triedAddrs[h] {
				nextAddr = h
				break
			}
		}
		if nextAddr != "" {
			triedAddrs[c.upstreamAddr] = true
			c.upstreamAddr = nextAddr
			select {
			case <-c.stopCh:
				return
			default:
			}
			continue
		}

		// Exhausted hints — reset to original address and apply backoff.
		c.upstreamAddr = originalAddr
		triedAddrs = map[string]bool{}

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

func (c *Client) connect() ([]string, error) {
	conn, err := net.DialTimeout("tcp", c.upstreamAddr, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	br, err := c.handshake(conn)
	if err != nil {
		return nil, err
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
func (c *Client) handshake(conn net.Conn) (*bufio.Reader, error) {
	chanIDHex := hex.EncodeToString(c.channelID[:])

	// 1. Send HTTP GET /channel/<id> with PCP upgrade header.
	req := fmt.Sprintf("GET /channel/%s HTTP/1.0\r\nHost: %s\r\nx-peercast-pcp: 1\r\nx-peercast-pos: 0\r\n\r\n", chanIDHex, c.upstreamAddr)
	if _, err := io.WriteString(conn, req); err != nil {
		return nil, fmt.Errorf("write GET: %w", err)
	}

	// 2. Send helo atom.
	// Note: the pcp\n PCP_CONNECT magic is sent only for direct TCP connections
	// (not HTTP-upgraded /channel/ requests), so it is intentionally omitted here.
	helo := pcp.NewParentAtom(pcp.PCPHelo,
		pcp.NewStringAtom(pcp.PCPHeloAgent, version.AgentName),
		pcp.NewIDAtom(pcp.PCPHeloSessionID, c.sessionID),
		pcp.NewIntAtom(pcp.PCPHeloVersion, version.PCPVersion),
		pcp.NewShortAtom(pcp.PCPHeloPort, c.listenPort),
	)
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

	slog.Info("relay: connected", "addr", c.upstreamAddr, "channel", chanIDHex)
	return br, nil
}

func (c *Client) receiveLoop(conn net.Conn, br *bufio.Reader) ([]string, error) {
	var hitHosts []string
	for {
		select {
		case <-c.stopCh:
			return nil, nil
		default:
		}

		conn.SetReadDeadline(time.Now().Add(readTimeout))
		atom, err := pcp.ReadAtom(br)
		conn.SetReadDeadline(time.Time{})
		if err != nil {
			select {
			case <-c.stopCh:
				return nil, nil
			default:
			}
			return nil, fmt.Errorf("read atom: %w", err)
		}

		switch atom.Tag {
		case pcp.PCPChan:
			c.handleChan(atom)
		case pcp.PCPHost:
			if addr := parseRelayHost(atom); addr != "" {
				hitHosts = append(hitHosts, addr)
			}
		case pcp.PCPQuit:
			code, _ := atom.GetInt()
			return hitHosts, fmt.Errorf("quit from upstream (code %d)", code)
		}
	}
}

const (
	// pcpHostFlagsRelay is set when the node can forward the stream.
	pcpHostFlagsRelay = byte(0x02)
	// pcpHostFlagsPush is set when the node is firewalled and needs a GIV
	// (push) to accept incoming connections. peercast-mi does not implement
	// GIV, so these nodes are skipped.
	pcpHostFlagsPush = byte(0x08)
)

// parseRelayHost extracts a "host:port" string from a PCPHost atom.
//
// A PCPHost atom contains TWO consecutive IP/port pairs: rhost[0] is the
// external (public) address, rhost[1] is the internal (local) address.
// We return the first non-unspecified, non-loopback address found.
//
// Returns "" when the host cannot be used (firewalled / no relay flag /
// no valid address).
func parseRelayHost(atom *pcp.Atom) string {
	// Collect up to two IP/port pairs in order.
	var ips [2]uint32
	var ports [2]uint16
	ipIdx, portIdx := 0, 0
	var flags byte

	for _, child := range atom.Children() {
		switch child.Tag {
		case pcp.PCPHostIP:
			if ipIdx < 2 {
				if v, err := child.GetInt(); err == nil {
					ips[ipIdx] = v
					ipIdx++
				}
			}
		case pcp.PCPHostPort:
			if portIdx < 2 {
				if v, err := child.GetShort(); err == nil {
					ports[portIdx] = v
					portIdx++
				}
			}
		case pcp.PCPHostFlags1:
			if v, err := child.GetByte(); err == nil {
				flags = v
			}
		}
	}

	// Skip firewalled nodes (PUSH flag) -- they require GIV which is not implemented.
	if flags&pcpHostFlagsPush != 0 {
		return ""
	}
	if flags&pcpHostFlagsRelay == 0 {
		return ""
	}

	// Try each IP/port pair in order; return the first usable address.
	// PCP stores IPv4 via writeInt (little-endian on wire), so GetInt() gives
	// the big-endian (network-byte-order) value -- restore it with BigEndian.
	for i := 0; i < 2; i++ {
		if ports[i] == 0 {
			continue
		}
		ip := pcp.IPv4FromUint32(ips[i])
		if ip.IsUnspecified() || ip.IsLoopback() {
			continue
		}
		return fmt.Sprintf("%s:%d", ip, ports[i])
	}
	return ""
}

func (c *Client) handleChan(atom *pcp.Atom) {
	for _, child := range atom.Children() {
		switch child.Tag {
		case pcp.PCPChanBCID:
			if bcID, err := child.GetID(); err == nil {
				c.ch.SetBroadcastID(bcID)
			}
		case pcp.PCPChanInfo:
			c.ch.SetInfo(parseChanInfo(child))
		case pcp.PCPChanTrack:
			c.ch.SetTrack(parseChanTrack(child))
		case pcp.PCPChanPkt:
			c.handlePkt(child)
		}
	}
}

func (c *Client) handlePkt(pkt *pcp.Atom) {
	typeAtom := pkt.FindChild(pcp.PCPChanPktType)
	posAtom := pkt.FindChild(pcp.PCPChanPktPos)
	if typeAtom == nil || posAtom == nil {
		return
	}
	pktType, err := typeAtom.GetID4()
	if err != nil {
		return
	}
	pos, err := posAtom.GetInt()
	if err != nil {
		return
	}

	switch pktType {
	case pktTypeHead:
		if dataAtom := pkt.FindChild(pcp.PCPChanPktData); dataAtom != nil {
			c.ch.SetHeader(dataAtom.Data())
		}
	case pktTypeData:
		dataAtom := pkt.FindChild(pcp.PCPChanPktData)
		if dataAtom == nil {
			return
		}
		var contFlags byte
		if contAtom := pkt.FindChild(pcp.PCPChanPktContinuation); contAtom != nil {
			if b, err := contAtom.GetByte(); err == nil {
				contFlags = b
			}
		}
		c.ch.Write(dataAtom.Data(), pos, contFlags)
	}
}

func parseChanInfo(a *pcp.Atom) channel.ChannelInfo {
	return channel.ChannelInfoFromPCP(pcp.ParseChanInfo(a))
}

func parseChanTrack(a *pcp.Atom) channel.TrackInfo {
	return channel.TrackInfoFromPCP(pcp.ParseChanTrack(a))
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
	return pcp.IPv4ToUint32(tcp.IP), uint16(tcp.Port)
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
