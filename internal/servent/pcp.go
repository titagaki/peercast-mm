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

	"github.com/titagaki/peercast-mi/internal/channel"
	"github.com/titagaki/peercast-mi/internal/version"
)

const (
	outputQueueTimeout = 5 * time.Second
	pcpWriteTimeout    = 10 * time.Second
)

// PCPOutputStream sends PCP stream data to a downstream relay node.
type PCPOutputStream struct {
	outputBase
	br         *bufio.Reader
	sessionID  pcp.GnuID
	peerID     pcp.GnuID // 下流ピアの session ID（ループ防止用）
	ch         *channel.Channel
	bcstCh     chan *pcp.Atom
	globalIP   uint32
	listenPort uint16
}

func newPCPOutputStream(conn *countingConn, br *bufio.Reader, sessionID pcp.GnuID, ch *channel.Channel, id int, globalIP uint32, listenPort uint16) *PCPOutputStream {
	o := &PCPOutputStream{
		outputBase: newOutputBase(conn, id),
		br:         br,
		sessionID:  sessionID,
		ch:         ch,
		bcstCh:     make(chan *pcp.Atom, 8),
		globalIP:   globalIP,
		listenPort: listenPort,
	}
	o.onClose = func() { conn.Close() }
	return o
}

// Type implements channel.OutputStream.
func (o *PCPOutputStream) Type() channel.OutputStreamType { return channel.OutputStreamPCP }

// PeerID implements channel.OutputStream.
func (o *PCPOutputStream) PeerID() pcp.GnuID { return o.peerID }

// SendBcst enqueues a bcst atom for forwarding to this downstream peer.
func (o *PCPOutputStream) SendBcst(atom *pcp.Atom) {
	select {
	case o.bcstCh <- atom:
	default:
	}
}

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

	go o.readLoop()
	o.streamLoop(startPos)
}

// handshake processes the HTTP GET and helo/oleh exchange.
// It returns the requested start position from the x-peercast-pos header (0 if absent).
//
// PCP over HTTP の場合、クライアントは HTTP 200 受信後に pcp\n マジックを送らず
// 直接 helo アトムを送る（peercast-yt channel.cpp "don't need PCP_CONNECT here"）。
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

	// Send HTTP 200 OK before PCP handshake (PeerCastStation 互換).
	resp := "HTTP/1.0 200 OK\r\nContent-Type: application/x-peercast-pcp\r\n\r\n"
	if _, err := io.WriteString(o.conn, resp); err != nil {
		return 0, fmt.Errorf("write HTTP response: %w", err)
	}

	// Read helo.
	heloAtom, err := pcp.ReadAtom(o.br)
	if err != nil {
		return 0, fmt.Errorf("read helo: %w", err)
	}
	if heloAtom.Tag != pcp.PCPHelo {
		return 0, fmt.Errorf("expected helo, got %s", heloAtom.Tag)
	}

	// Validate helo: sid and ver are mandatory per PCP spec.
	sidAtom := heloAtom.FindChild(pcp.PCPHeloSessionID)
	if sidAtom == nil {
		o.sendQuit(pcp.PCPErrorQuit + pcp.PCPErrorNotIdentified)
		return 0, fmt.Errorf("no session ID in helo")
	}
	peerID, err := sidAtom.GetID()
	if err != nil {
		o.sendQuit(pcp.PCPErrorQuit + pcp.PCPErrorNotIdentified)
		return 0, fmt.Errorf("invalid session ID: %w", err)
	}
	o.peerID = peerID
	if peerID == o.sessionID {
		o.sendQuit(pcp.PCPErrorQuit + pcp.PCPErrorLoopback)
		return 0, fmt.Errorf("loopback connection")
	}
	var zeroID pcp.GnuID
	if peerID == zeroID {
		o.sendQuit(pcp.PCPErrorQuit + pcp.PCPErrorNotIdentified)
		return 0, fmt.Errorf("not identified")
	}
	verAtom := heloAtom.FindChild(pcp.PCPHeloVersion)
	if verAtom == nil {
		o.sendQuit(pcp.PCPErrorQuit + pcp.PCPErrorBadAgent)
		return 0, fmt.Errorf("no version in helo")
	}
	if v, err := verAtom.GetInt(); err == nil && v < 1200 {
		o.sendQuit(pcp.PCPErrorQuit + pcp.PCPErrorBadAgent)
		return 0, fmt.Errorf("bad agent version %d", v)
	}

	// Determine remote port from helo: ping (active check) > port (claimed).
	remoteIP := ipToUint32(o.conn.RemoteAddr())
	var remotePort uint16
	if pingAtom := heloAtom.FindChild(pcp.PCPHeloPing); pingAtom != nil {
		if pingPort, err := pingAtom.GetShort(); err == nil && pingPort != 0 {
			remoteAddr := o.conn.RemoteAddr().(*net.TCPAddr)
			if pingHost(remoteAddr.IP, pingPort, peerID, o.sessionID) {
				remotePort = pingPort
			}
		}
	} else if portAtom := heloAtom.FindChild(pcp.PCPHeloPort); portAtom != nil {
		if p, err := portAtom.GetShort(); err == nil {
			remotePort = p
		}
	}

	// Send oleh.
	oleh := pcp.NewParentAtom(pcp.PCPOleh,
		pcp.NewStringAtom(pcp.PCPHeloAgent, version.AgentName),
		pcp.NewIDAtom(pcp.PCPHeloSessionID, o.sessionID),
		pcp.NewIntAtom(pcp.PCPHeloVersion, version.PCPVersion),
		pcp.NewIntAtom(pcp.PCPHeloRemoteIP, remoteIP),
		pcp.NewShortAtom(pcp.PCPHeloPort, remotePort),
	)
	if err := oleh.Write(o.conn); err != nil {
		return 0, fmt.Errorf("write oleh: %w", err)
	}

	return startPos, nil
}

// sendInitial sends chan (info, trck, head pkt) and host atoms.
func (o *PCPOutputStream) sendInitial() error {
	info := o.ch.Info()
	track := o.ch.Track()
	header, headerPos := o.ch.Buffer.Header()

	chanAtom := buildChanAtom(o.ch.ID, o.ch.BroadcastID(), info, track, header, headerPos)
	hostAtom := o.buildHostAtom()

	o.conn.SetWriteDeadline(time.Now().Add(pcpWriteTimeout))
	if err := chanAtom.Write(o.conn); err != nil {
		o.conn.SetWriteDeadline(time.Time{})
		return err
	}
	err := hostAtom.Write(o.conn)
	o.conn.SetWriteDeadline(time.Time{})
	return err
}

func (o *PCPOutputStream) buildHostAtom() *pcp.Atom {
	buf := o.ch.Buffer
	flags := byte(pcp.PCPHostFlags1Relay | pcp.PCPHostFlags1Recv | pcp.PCPHostFlags1CIN)
	if o.globalIP != 0 {
		flags |= pcp.PCPHostFlags1Direct
	}
	if o.ch.IsBroadcasting() {
		flags |= pcp.PCPHostFlags1Tracker
	}
	return pcp.NewParentAtom(pcp.PCPHost,
		pcp.NewIDAtom(pcp.PCPHostID, o.sessionID),
		pcp.NewIntAtom(pcp.PCPHostIP, o.globalIP),
		pcp.NewShortAtom(pcp.PCPHostPort, o.listenPort),
		pcp.NewIntAtom(pcp.PCPHostNumListeners, uint32(o.ch.NumListeners())),
		pcp.NewIntAtom(pcp.PCPHostNumRelays, uint32(o.ch.NumRelays())),
		pcp.NewIntAtom(pcp.PCPHostUptime, o.ch.UptimeSeconds()),
		pcp.NewIntAtom(pcp.PCPHostOldPos, buf.OldestPos()),
		pcp.NewIntAtom(pcp.PCPHostNewPos, buf.NewestPos()),
		pcp.NewIDAtom(pcp.PCPHostChanID, o.ch.ID),
		pcp.NewByteAtom(pcp.PCPHostFlags1, flags),
		pcp.NewIntAtom(pcp.PCPHostVersion, version.PCPVersion),
		pcp.NewIntAtom(pcp.PCPHostVersionVP, version.PCPVersionVP),
		pcp.NewBytesAtom(pcp.PCPHostVersionExPrefix, []byte(version.ExPrefix)),
		pcp.NewShortAtom(pcp.PCPHostVersionExNumber, version.ExNumber()),
	)
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
		// Process notifications non-blockingly.
		o.drainNotifications()

		// Send buffered data packets.
		sigCh := o.ch.Buffer.Signal()
		packets := o.ch.Buffer.Since(pos)

		if len(packets) > 0 {
			for _, pkt := range packets {
				if waitingForKeyframe && pkt.ContFlags != 0 {
					pos = pkt.Pos + uint32(len(pkt.Data))
					continue
				}
				waitingForKeyframe = false

				pktAtom := pcp.NewParentAtom(pcp.PCPChanPkt,
					pcp.NewID4Atom(pcp.PCPChanPktType, pcp.NewID4("data")),
					pcp.NewIntAtom(pcp.PCPChanPktPos, pkt.Pos),
					pcp.NewBytesAtom(pcp.PCPChanPktData, pkt.Data),
					pcp.NewByteAtom(pcp.PCPChanPktContinuation, pkt.ContFlags),
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

		// No data available — not a queue stall; reset the timer.
		stallTimer.Reset(outputQueueTimeout)
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
		case atom := <-o.bcstCh:
			o.sendBcstAtom(atom)
		case <-stallTimer.C:
			slog.Info("pcp: queue timeout, closing", "remote", o.remoteAddr, "id", o.id)
			return
		}
	}
}

// drainNotifications processes any pending info/track/header/bcst notifications
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
		case atom := <-o.bcstCh:
			o.sendBcstAtom(atom)
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
	o.conn.SetWriteDeadline(time.Now().Add(pcpWriteTimeout))
	atom.Write(o.conn)
	o.conn.SetWriteDeadline(time.Time{})
}

func (o *PCPOutputStream) sendTrackUpdate() {
	track := o.ch.Track()
	atom := pcp.NewParentAtom(pcp.PCPChan,
		pcp.NewIDAtom(pcp.PCPChanID, o.ch.ID),
		buildChanTrackAtom(track),
	)
	o.conn.SetWriteDeadline(time.Now().Add(pcpWriteTimeout))
	atom.Write(o.conn)
	o.conn.SetWriteDeadline(time.Time{})
}

func (o *PCPOutputStream) sendHeaderUpdate() {
	header, hpos := o.ch.Buffer.Header()
	atom := pcp.NewParentAtom(pcp.PCPChan,
		pcp.NewIDAtom(pcp.PCPChanID, o.ch.ID),
		buildPktHeadAtom(header, hpos),
	)
	o.conn.SetWriteDeadline(time.Now().Add(pcpWriteTimeout))
	atom.Write(o.conn)
	o.conn.SetWriteDeadline(time.Time{})
}

func (o *PCPOutputStream) sendBcstAtom(atom *pcp.Atom) {
	o.conn.SetWriteDeadline(time.Now().Add(pcpWriteTimeout))
	atom.Write(o.conn)
	o.conn.SetWriteDeadline(time.Time{})
}

// readLoop reads atoms from the downstream peer in a dedicated goroutine.
// o.br (not o.conn) must be used because o.br may have buffered bytes from the handshake.
// On any read error or quit atom, it closes the stream so streamLoop exits.
func (o *PCPOutputStream) readLoop() {
	for {
		a, err := pcp.ReadAtom(o.br)
		if err != nil {
			slog.Debug("pcp: read error from downstream", "remote", o.remoteAddr, "id", o.id, "err", err)
			o.Close()
			return
		}
		switch a.Tag {
		case pcp.PCPBcst:
			o.forwardBcst(a)
		case pcp.PCPQuit:
			code := uint32(0)
			if v, err := a.GetInt(); err == nil {
				code = v
			}
			slog.Debug("pcp: quit received from downstream", "remote", o.remoteAddr, "id", o.id, "code", code)
			o.Close()
			return
		default:
			slog.Debug("pcp: unknown atom from downstream", "remote", o.remoteAddr, "id", o.id, "tag", a.Tag)
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
	// Check dest: if set and not us, forward without processing.
	if dest := a.FindChild(pcp.PCPBcstDest); dest != nil {
		id, err := dest.GetID()
		if err == nil && id == o.sessionID {
			return // addressed to us, don't forward
		}
	}
	// Rebuild bcst with decremented TTL and incremented hops.
	forwarded := rebuildBcst(a, ttl)
	o.ch.Broadcast(o, forwarded)
	slog.Debug("pcp: bcst forwarded from downstream", "remote", o.remoteAddr, "id", o.id, "ttl", ttl-1)
}

// rebuildBcst creates a new bcst atom with TTL decremented by 1 and hops incremented by 1.
func rebuildBcst(a *pcp.Atom, ttl byte) *pcp.Atom {
	var children []*pcp.Atom
	for _, c := range a.Children() {
		switch c.Tag {
		case pcp.PCPBcstTTL:
			children = append(children, pcp.NewByteAtom(pcp.PCPBcstTTL, ttl-1))
		case pcp.PCPBcstHops:
			hops, err := c.GetByte()
			if err == nil {
				children = append(children, pcp.NewByteAtom(pcp.PCPBcstHops, hops+1))
			} else {
				children = append(children, c)
			}
		default:
			children = append(children, c)
		}
	}
	return pcp.NewParentAtom(pcp.PCPBcst, children...)
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
		pcp.NewBytesAtom(pcp.PCPChanPktData, header),
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

const pingTimeout = 2 * time.Second

// pingHost performs a firewall reachability check by connecting to the remote
// host on the specified port, sending pcp\n + helo, and checking if the oleh
// session ID matches the expected peerID.
func pingHost(remoteIP net.IP, port uint16, peerID, mySessionID pcp.GnuID) bool {
	addr := net.JoinHostPort(remoteIP.String(), strconv.Itoa(int(port)))
	conn, err := net.DialTimeout("tcp", addr, pingTimeout)
	if err != nil {
		slog.Debug("ping: dial failed", "addr", addr, "err", err)
		return false
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(pingTimeout))

	// Send pcp\n magic: tag "pcp\n" + size 4 (LE) + version 1 (LE).
	var magic [12]byte
	copy(magic[0:4], "pcp\n")
	magic[4] = 4 // size LE
	magic[8] = 1 // version LE
	if _, err := conn.Write(magic[:]); err != nil {
		return false
	}

	// Send helo with our session ID.
	helo := pcp.NewParentAtom(pcp.PCPHelo,
		pcp.NewIDAtom(pcp.PCPHeloSessionID, mySessionID),
	)
	if err := helo.Write(conn); err != nil {
		return false
	}

	// Read oleh.
	br := bufio.NewReader(conn)
	oleh, err := pcp.ReadAtom(br)
	if err != nil || oleh.Tag != pcp.PCPOleh {
		return false
	}
	sidAtom := oleh.FindChild(pcp.PCPHeloSessionID)
	if sidAtom == nil {
		return false
	}
	sid, err := sidAtom.GetID()
	if err != nil {
		return false
	}
	if sid == peerID {
		slog.Debug("ping: succeeded", "addr", addr)
		return true
	}
	slog.Debug("ping: session ID mismatch", "addr", addr)
	return false
}
