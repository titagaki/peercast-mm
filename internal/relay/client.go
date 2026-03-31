package relay

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/titagaki/peercast-pcp/pcp"

	"github.com/titagaki/peercast-mm/internal/channel"
	"github.com/titagaki/peercast-mm/internal/version"
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

// Client connects to an upstream PeerCast node and writes the received stream
// into a local channel, reconnecting on failure.
type Client struct {
	upstreamAddr string
	channelID    pcp.GnuID
	sessionID    pcp.GnuID
	ch           *channel.Channel

	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once
}

// New creates a new relay Client.
func New(upstreamAddr string, channelID, sessionID pcp.GnuID, ch *channel.Channel) *Client {
	return &Client{
		upstreamAddr: upstreamAddr,
		channelID:    channelID,
		sessionID:    sessionID,
		ch:           ch,
		stopCh:       make(chan struct{}),
		doneCh:       make(chan struct{}),
	}
}

// Run connects to the upstream node and runs the receive loop, reconnecting on
// failure with exponential backoff. It blocks until Stop is called.
func (c *Client) Run() {
	defer close(c.doneCh)
	delay := retryInitial
	for {
		if err := c.connect(); err != nil {
			slog.Error("relay: connection error", "addr", c.upstreamAddr, "err", err)
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

func (c *Client) connect() error {
	chanIDHex := hex.EncodeToString(c.channelID[:])

	conn, err := net.DialTimeout("tcp", c.upstreamAddr, dialTimeout)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	// 1. Send HTTP GET /channel/<id>.
	req := fmt.Sprintf("GET /channel/%s HTTP/1.0\r\nHost: %s\r\n\r\n", chanIDHex, c.upstreamAddr)
	if _, err := io.WriteString(conn, req); err != nil {
		return fmt.Errorf("write GET: %w", err)
	}

	// 2. Send pcp\n magic: tag "pcp\n" + size 4 (LE) + version 1 (LE).
	var magic [12]byte
	copy(magic[0:4], "pcp\n")
	binary.LittleEndian.PutUint32(magic[4:8], 4)
	binary.LittleEndian.PutUint32(magic[8:12], 1)
	if _, err := conn.Write(magic[:]); err != nil {
		return fmt.Errorf("write pcp magic: %w", err)
	}

	// 3. Send helo atom.
	helo := pcp.NewParentAtom(pcp.PCPHelo,
		pcp.NewStringAtom(pcp.PCPHeloAgent, version.AgentName),
		pcp.NewIDAtom(pcp.PCPHeloSessionID, c.sessionID),
		pcp.NewIntAtom(pcp.PCPHeloVersion, version.PCPVersion),
	)
	if err := helo.Write(conn); err != nil {
		return fmt.Errorf("write helo: %w", err)
	}

	br := bufio.NewReader(conn)

	// 4. Read HTTP response headers (skip until blank line).
	statusCode, err := readHTTPStatus(br)
	if err != nil {
		return fmt.Errorf("read HTTP response: %w", err)
	}
	if statusCode != 200 {
		return fmt.Errorf("upstream returned HTTP %d", statusCode)
	}

	// 5. Read oleh atom.
	oleh, err := pcp.ReadAtom(br)
	if err != nil {
		return fmt.Errorf("read oleh: %w", err)
	}
	if oleh.Tag != pcp.PCPOleh {
		return fmt.Errorf("expected oleh, got %s", oleh.Tag)
	}

	slog.Info("relay: connected", "addr", c.upstreamAddr, "channel", chanIDHex)

	// 6. Receive loop.
	return c.receiveLoop(conn, br)
}

func (c *Client) receiveLoop(conn net.Conn, br *bufio.Reader) error {
	for {
		select {
		case <-c.stopCh:
			return nil
		default:
		}

		conn.SetReadDeadline(time.Now().Add(readTimeout))
		atom, err := pcp.ReadAtom(br)
		conn.SetReadDeadline(time.Time{})
		if err != nil {
			select {
			case <-c.stopCh:
				return nil
			default:
			}
			return fmt.Errorf("read atom: %w", err)
		}

		switch atom.Tag {
		case pcp.PCPChan:
			c.handleChan(atom)
		case pcp.PCPQuit:
			code, _ := atom.GetInt()
			return fmt.Errorf("quit from upstream (code %d)", code)
		}
	}
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
		cont := false
		if contAtom := pkt.FindChild(pcp.PCPChanPktContinuation); contAtom != nil {
			b, err := contAtom.GetByte()
			cont = err == nil && b != 0
		}
		c.ch.Write(dataAtom.Data(), pos, cont)
	}
}

func parseChanInfo(a *pcp.Atom) channel.ChannelInfo {
	var info channel.ChannelInfo
	for _, child := range a.Children() {
		switch child.Tag {
		case pcp.PCPChanInfoName:
			info.Name = child.GetString()
		case pcp.PCPChanInfoURL:
			info.URL = child.GetString()
		case pcp.PCPChanInfoDesc:
			info.Desc = child.GetString()
		case pcp.PCPChanInfoComment:
			info.Comment = child.GetString()
		case pcp.PCPChanInfoGenre:
			info.Genre = child.GetString()
		case pcp.PCPChanInfoType:
			info.Type = child.GetString()
		case pcp.PCPChanInfoBitrate:
			if v, err := child.GetInt(); err == nil {
				info.Bitrate = v
			}
		}
	}
	switch info.Type {
	case "FLV":
		info.MIMEType = "video/x-flv"
		info.Ext = ".flv"
	}
	return info
}

func parseChanTrack(a *pcp.Atom) channel.TrackInfo {
	var track channel.TrackInfo
	for _, child := range a.Children() {
		switch child.Tag {
		case pcp.PCPChanTrackTitle:
			track.Title = child.GetString()
		case pcp.PCPChanTrackCreator:
			track.Creator = child.GetString()
		case pcp.PCPChanTrackURL:
			track.URL = child.GetString()
		case pcp.PCPChanTrackAlbum:
			track.Album = child.GetString()
		}
	}
	return track
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
