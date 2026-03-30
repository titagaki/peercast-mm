package servent

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/titagaki/peercast-pcp/pcp"

	"github.com/titagaki/peercast-mm/internal/channel"
	"github.com/titagaki/peercast-mm/internal/version"
)

const (
	outputQueueTimeout = 5 * time.Second
	pcpWriteTimeout    = 10 * time.Second
)

// PCPOutputStream sends PCP stream data to a downstream relay node.
type PCPOutputStream struct {
	outputBase
	br        *bufio.Reader
	sessionID pcp.GnuID
	ch        *channel.Channel
}

func newPCPOutputStream(conn *countingConn, br *bufio.Reader, sessionID pcp.GnuID, ch *channel.Channel, id int) *PCPOutputStream {
	return &PCPOutputStream{
		outputBase: newOutputBase(conn, id),
		br:         br,
		sessionID:  sessionID,
		ch:         ch,
	}
}

// Type implements channel.OutputStream.
func (o *PCPOutputStream) Type() channel.OutputStreamType { return channel.OutputStreamPCP }

func (o *PCPOutputStream) run() {
	defer slog.Info("pcp: relay disconnected", "remote", o.remoteAddr, "id", o.id)
	defer o.conn.Close()

	startPos, err := o.handshake()
	if err != nil {
		slog.Error("pcp: handshake error", "remote", o.remoteAddr, "id", o.id, "err", err)
		return
	}
	slog.Debug("pcp: handshake complete", "remote", o.remoteAddr, "id", o.id, "start_pos", startPos)

	if err := o.sendInitial(); err != nil {
		slog.Error("pcp: send initial error", "remote", o.remoteAddr, "id", o.id, "err", err)
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

	stallTimer := time.NewTimer(outputQueueTimeout)
	defer stallTimer.Stop()
	waitingForKeyframe := true

	for {
		// Drain incoming atoms from the peer (bcst forwarding).
		o.tryReadBcst()

		// Process notifications non-blockingly.
		o.drainNotifications()

		// Send buffered data packets.
		sigCh := o.ch.Buffer.Signal()
		packets := o.ch.Buffer.Since(pos)

		if len(packets) > 0 {
			for _, pkt := range packets {
				if waitingForKeyframe && pkt.Cont {
					pos = pkt.Pos + uint32(len(pkt.Data))
					continue
				}
				waitingForKeyframe = false

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
				o.conn.SetWriteDeadline(time.Now().Add(pcpWriteTimeout))
				if err := atom.Write(o.conn); err != nil {
					return
				}
				o.conn.SetWriteDeadline(time.Time{})
				pos = pkt.Pos + uint32(len(pkt.Data))
			}
			stallTimer.Reset(outputQueueTimeout)
			continue
		}

		// No data available — block until an event arrives.
		select {
		case <-o.closeCh:
			o.sendQuit(pcp.PCPErrorQuit + pcp.PCPErrorShutdown)
			return
		case <-sigCh:
			// New data written to buffer.
		case <-o.infoCh:
			o.sendInfoUpdate()
		case <-o.trackCh:
			o.sendTrackUpdate()
		case <-o.headerCh:
			o.sendHeaderUpdate()
		case <-stallTimer.C:
			slog.Info("pcp: queue timeout, closing", "remote", o.remoteAddr, "id", o.id)
			return
		}
	}
}

// drainNotifications processes any pending info/track/header notifications
// without blocking.
func (o *PCPOutputStream) drainNotifications() {
	for {
		select {
		case <-o.infoCh:
			o.sendInfoUpdate()
		case <-o.trackCh:
			o.sendTrackUpdate()
		case <-o.headerCh:
			o.sendHeaderUpdate()
		default:
			return
		}
	}
}

func (o *PCPOutputStream) sendInfoUpdate() {
	info := o.ch.Info()
	atom := pcp.NewParentAtom(pcp.PCPChan,
		pcp.NewIDAtom(pcp.PCPChanID, o.ch.ID),
		buildChanInfoAtom(info),
	)
	atom.Write(o.conn)
}

func (o *PCPOutputStream) sendTrackUpdate() {
	track := o.ch.Track()
	atom := pcp.NewParentAtom(pcp.PCPChan,
		pcp.NewIDAtom(pcp.PCPChanID, o.ch.ID),
		buildChanTrackAtom(track),
	)
	atom.Write(o.conn)
}

func (o *PCPOutputStream) sendHeaderUpdate() {
	header, hpos := o.ch.Buffer.Header()
	atom := pcp.NewParentAtom(pcp.PCPChan,
		pcp.NewIDAtom(pcp.PCPChanID, o.ch.ID),
		buildPktHeadAtom(header, hpos),
	)
	atom.Write(o.conn)
}

// tryReadBcst does a non-blocking read for bcst atoms from the downstream peer.
func (o *PCPOutputStream) tryReadBcst() {
	o.conn.SetReadDeadline(time.Now().Add(1 * time.Millisecond))
	if a, err := pcp.ReadAtom(o.conn); err == nil {
		if a.Tag == pcp.PCPBcst {
			o.forwardBcst(a)
		}
	}
	o.conn.SetReadDeadline(time.Time{})
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
	slog.Debug("pcp: bcst received from downstream", "remote", o.remoteAddr, "id", o.id, "ttl", ttl)
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
