package servent

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/titagaki/peercast-pcp/pcp"

	"github.com/titagaki/peercast-mm/internal/channel"
	"github.com/titagaki/peercast-mm/internal/version"
)

const (
	outputQueueTimeout = 5 * time.Second
	pollInterval       = 50 * time.Millisecond
)

// PCPOutputStream sends PCP stream data to a downstream relay node.
type PCPOutputStream struct {
	conn      net.Conn
	br        *bufio.Reader
	sessionID pcp.GnuID
	ch        *channel.Channel

	mu        sync.Mutex
	closed    bool
	headerCh  chan struct{}
	infoCh    chan struct{}
	trackCh   chan struct{}
	closeCh   chan struct{}
}

func newPCPOutputStream(conn net.Conn, br *bufio.Reader, sessionID pcp.GnuID, ch *channel.Channel) *PCPOutputStream {
	return &PCPOutputStream{
		conn:      conn,
		br:        br,
		sessionID: sessionID,
		ch:        ch,
		headerCh:  make(chan struct{}, 1),
		infoCh:    make(chan struct{}, 1),
		trackCh:   make(chan struct{}, 1),
		closeCh:   make(chan struct{}),
	}
}

// Type implements channel.OutputStream.
func (o *PCPOutputStream) Type() channel.OutputStreamType { return channel.OutputStreamPCP }

// NotifyHeader implements channel.OutputStream.
func (o *PCPOutputStream) NotifyHeader() { notify(o.headerCh) }

// NotifyInfo implements channel.OutputStream.
func (o *PCPOutputStream) NotifyInfo() { notify(o.infoCh) }

// NotifyTrack implements channel.OutputStream.
func (o *PCPOutputStream) NotifyTrack() { notify(o.trackCh) }

// Close implements channel.OutputStream.
func (o *PCPOutputStream) Close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.closed {
		o.closed = true
		close(o.closeCh)
		o.conn.Close()
	}
}

func (o *PCPOutputStream) run() {
	defer o.conn.Close()

	startPos, err := o.handshake()
	if err != nil {
		log.Printf("pcp servent: handshake error: %v", err)
		return
	}

	if err := o.sendInitial(); err != nil {
		log.Printf("pcp servent: send initial error: %v", err)
		return
	}

	o.streamLoop(startPos)
}

// handshake processes the HTTP GET, "pcp\n", and helo/oleh exchange.
// It returns the requested start position from the x-peercast-pos header (0 if absent).
func (o *PCPOutputStream) handshake() (startPos uint32, err error) {
	// Read HTTP request.
	req, err := http.ReadRequest(o.br)
	if err != nil {
		return 0, fmt.Errorf("read HTTP request: %w", err)
	}
	_ = req.Body.Close()

	// x-peercast-pos ヘッダーを読み取る
	if v := req.Header.Get("x-peercast-pos"); v != "" {
		if n, parseErr := strconv.ParseUint(v, 10, 32); parseErr == nil {
			startPos = uint32(n)
		}
	}

	// Read "pcp\n" magic atom: 4-byte tag + 4-byte length field + 4-byte version payload.
	pcpMagic := make([]byte, 12)
	if _, err := io.ReadFull(o.br, pcpMagic); err != nil {
		return 0, fmt.Errorf("read pcp magic: %w", err)
	}

	// Read helo.
	pcpConn := &pcp.Conn{}
	_ = pcpConn // We'll use pcp.ReadAtom directly on the buffered reader.

	heloAtom, err := pcp.ReadAtom(o.br)
	if err != nil {
		return 0, fmt.Errorf("read helo: %w", err)
	}
	if heloAtom.Tag != pcp.PCPHelo {
		return 0, fmt.Errorf("expected helo, got %s", heloAtom.Tag)
	}

	// Validate helo.
	if sid := heloAtom.FindChild(pcp.PCPHeloSessionID); sid != nil {
		id, err := sid.GetID()
		if err == nil {
			if id == o.sessionID {
				o.sendQuit(pcp.PCPErrorQuit + pcp.PCPErrorLoopback)
				return 0, fmt.Errorf("loopback connection")
			}
			var zero pcp.GnuID
			if id == zero {
				o.sendQuit(pcp.PCPErrorQuit + pcp.PCPErrorNotIdentified)
				return 0, fmt.Errorf("not identified")
			}
		}
	}
	if ver := heloAtom.FindChild(pcp.PCPHeloVersion); ver != nil {
		if v, err := ver.GetInt(); err == nil && v < 1200 {
			o.sendQuit(pcp.PCPErrorQuit + pcp.PCPErrorBadAgent)
			return 0, fmt.Errorf("bad agent version %d", v)
		}
	}

	// Send HTTP 200 OK first; oleh goes in the response body.
	resp := "HTTP/1.0 200 OK\r\nContent-Type: application/x-peercast-pcp\r\n\r\n"
	if _, err := io.WriteString(o.conn, resp); err != nil {
		return 0, fmt.Errorf("write HTTP response: %w", err)
	}

	// Send oleh.
	remoteIP := ipToUint32(o.conn.RemoteAddr())
	oleh := pcp.NewParentAtom(pcp.PCPOleh,
		pcp.NewStringAtom(pcp.PCPHeloAgent, version.AgentName),
		pcp.NewIDAtom(pcp.PCPHeloSessionID, o.sessionID),
		pcp.NewIntAtom(pcp.PCPHeloVersion, version.PCPVersion),
		pcp.NewIntAtom(pcp.PCPHeloRemoteIP, remoteIP),
	)
	if err := oleh.Write(o.conn); err != nil {
		return 0, fmt.Errorf("write oleh: %w", err)
	}

	return startPos, nil
}

// sendInitial sends chan > info, trck, and the head pkt.
func (o *PCPOutputStream) sendInitial() error {
	info := o.ch.Info()
	track := o.ch.Track()
	header, headerPos := o.ch.Buffer.Header()

	chanAtom := buildChanAtom(o.ch.ID, o.ch.BroadcastID, info, track, header, headerPos)
	return chanAtom.Write(o.conn)
}

// streamLoop continuously sends buffered content packets to the peer.
// reqPos は x-peercast-pos で指定された開始位置 (0 = ヘッダー位置から開始)。
func (o *PCPOutputStream) streamLoop(reqPos uint32) {
	_, hpos := o.ch.Buffer.Header()
	pos := hpos
	// reqPos == 0 は「未指定」と同義に扱う。ストリーム開始直後に pos=0 を送ってくる
	// クライアントがいても hpos == 0 のはずなので実害はない。
	if reqPos > 0 {
		oldest := o.ch.Buffer.OldestPos()
		if reqPos >= oldest {
			pos = reqPos
		} else {
			pos = oldest
		}
	}

	lastSend := time.Now()

	for {
		select {
		case <-o.closeCh:
			o.sendQuit(pcp.PCPErrorQuit + pcp.PCPErrorShutdown)
			return
		default:
		}

		// Check for queue stall.
		if time.Since(lastSend) > outputQueueTimeout {
			log.Printf("pcp servent: queue timeout, closing")
			return
		}

		// Drain incoming atoms from the peer (bcst forwarding).
		o.conn.SetReadDeadline(time.Now().Add(1 * time.Millisecond))
		if a, err := pcp.ReadAtom(o.conn); err == nil {
			if a.Tag == pcp.PCPBcst {
				o.forwardBcst(a)
			}
		}
		o.conn.SetReadDeadline(time.Time{})

		// Handle notifications.
		select {
		case <-o.infoCh:
			info := o.ch.Info()
			chanInfo := buildChanInfoAtom(info)
			atom := pcp.NewParentAtom(pcp.PCPChan,
				pcp.NewIDAtom(pcp.PCPChanID, o.ch.ID),
				chanInfo,
			)
			atom.Write(o.conn)
		default:
		}
		select {
		case <-o.trackCh:
			track := o.ch.Track()
			chanTrack := buildChanTrackAtom(track)
			atom := pcp.NewParentAtom(pcp.PCPChan,
				pcp.NewIDAtom(pcp.PCPChanID, o.ch.ID),
				chanTrack,
			)
			atom.Write(o.conn)
		default:
		}
		select {
		case <-o.headerCh:
			header, hpos := o.ch.Buffer.Header()
			pktAtom := buildPktHeadAtom(header, hpos)
			atom := pcp.NewParentAtom(pcp.PCPChan,
				pcp.NewIDAtom(pcp.PCPChanID, o.ch.ID),
				pktAtom,
			)
			atom.Write(o.conn)
		default:
		}

		// Send buffered data packets.
		packets := o.ch.Buffer.Since(pos)
		if len(packets) == 0 {
			time.Sleep(pollInterval)
			continue
		}

		for _, pkt := range packets {
			cont := byte(0)
			if pkt.Cont {
				cont = 1
			}
			pktAtom := pcp.NewParentAtom(pcp.PCPChanPkt,
				pcp.NewID4Atom(pcp.PCPChanPktType, pcp.NewID4("data")),
				pcp.NewIntAtom(pcp.PCPChanPktPos, pkt.Pos),
				pcp.NewBytesAtom(pcp.PCPChanPktData, pkt.Data),
				pcp.NewByteAtom(pcp.PCPChanPktContinuation, cont),
			)
			atom := pcp.NewParentAtom(pcp.PCPChan,
				pcp.NewIDAtom(pcp.PCPChanID, o.ch.ID),
				pktAtom,
			)
			if err := atom.Write(o.conn); err != nil {
				return
			}
			pos = pkt.Pos + uint32(len(pkt.Data))
			lastSend = time.Now()
		}
	}
}

func (o *PCPOutputStream) forwardBcst(a *pcp.Atom) {
	// TTL decrement: find ttl child.
	ttlAtom := a.FindChild(pcp.PCPBcstTTL)
	if ttlAtom == nil {
		return
	}
	ttl, err := ttlAtom.GetByte()
	if err != nil || ttl == 0 {
		return
	}
	// Check from == our sessionID (loop prevention).
	if from := a.FindChild(pcp.PCPBcstFrom); from != nil {
		id, err := from.GetID()
		if err == nil && id == o.sessionID {
			return
		}
	}
	// We don't re-broadcast here; the Listener would fan-out.
	// For now, silently consume.
}

func (o *PCPOutputStream) sendQuit(code uint32) {
	q := pcp.NewIntAtom(pcp.PCPQuit, code)
	q.Write(o.conn)
}

// ---------------------------------------------------------------------------
// Atom builders
// ---------------------------------------------------------------------------

func buildChanAtom(chanID, bcID pcp.GnuID, info channel.ChannelInfo, track channel.TrackInfo, header []byte, headerPos uint32) *pcp.Atom {
	pktAtom := buildPktHeadAtom(header, headerPos)
	return pcp.NewParentAtom(pcp.PCPChan,
		pcp.NewIDAtom(pcp.PCPChanID, chanID),
		pcp.NewIDAtom(pcp.PCPChanBCID, bcID),
		buildChanInfoAtom(info),
		buildChanTrackAtom(track),
		pktAtom,
	)
}

func buildChanInfoAtom(info channel.ChannelInfo) *pcp.Atom {
	return pcp.NewParentAtom(pcp.PCPChanInfo,
		pcp.NewStringAtom(pcp.PCPChanInfoName, info.Name),
		pcp.NewStringAtom(pcp.PCPChanInfoURL, info.URL),
		pcp.NewStringAtom(pcp.PCPChanInfoDesc, info.Desc),
		pcp.NewStringAtom(pcp.PCPChanInfoComment, info.Comment),
		pcp.NewStringAtom(pcp.PCPChanInfoGenre, info.Genre),
		pcp.NewStringAtom(pcp.PCPChanInfoType, info.Type),
		pcp.NewIntAtom(pcp.PCPChanInfoBitrate, info.Bitrate),
	)
}

func buildChanTrackAtom(track channel.TrackInfo) *pcp.Atom {
	return pcp.NewParentAtom(pcp.PCPChanTrack,
		pcp.NewStringAtom(pcp.PCPChanTrackTitle, track.Title),
		pcp.NewStringAtom(pcp.PCPChanTrackCreator, track.Creator),
		pcp.NewStringAtom(pcp.PCPChanTrackURL, track.URL),
		pcp.NewStringAtom(pcp.PCPChanTrackAlbum, track.Album),
	)
}

func buildPktHeadAtom(header []byte, pos uint32) *pcp.Atom {
	return pcp.NewParentAtom(pcp.PCPChanPkt,
		pcp.NewID4Atom(pcp.PCPChanPktType, pcp.NewID4("head")),
		pcp.NewIntAtom(pcp.PCPChanPktPos, pos),
		pcp.NewBytesAtom(pcp.PCPChanPktHead, header),
	)
}

func notify(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func ipToUint32(addr net.Addr) uint32 {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return 0
	}
	ip := tcp.IP.To4()
	if ip == nil {
		return 0
	}
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

// ensure unused import doesn't cause errors
var _ = strings.NewReader
