package jsonrpc

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/titagaki/peercast-pcp/pcp"

	"github.com/titagaki/peercast-mm/internal/channel"
	"github.com/titagaki/peercast-mm/internal/config"
	"github.com/titagaki/peercast-mm/internal/version"
)

// YPBumper is the subset of yp.Client that the JSON-RPC server needs.
type YPBumper interface {
	Bump()
}

// Server handles JSON-RPC 2.0 requests at POST /api/1.
type Server struct {
	sessionID pcp.GnuID
	mgr       *channel.Manager
	cfg       *config.Config
	ypClient  YPBumper // may be nil
}

// New creates a new JSON-RPC Server.
func New(sessionID pcp.GnuID, mgr *channel.Manager, cfg *config.Config, ypClient YPBumper) *Server {
	return &Server{
		sessionID: sessionID,
		mgr:       mgr,
		cfg:       cfg,
		ypClient:  ypClient,
	}
}

// Handler returns an http.Handler for POST /api/1.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeRPCError(w, nil, errCodeParse, "parse error")
			return
		}

		result, rpcErr := s.dispatch(req.Method, req.Params)

		w.Header().Set("Content-Type", "application/json")
		resp := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      req.ID,
		}
		if rpcErr != nil {
			resp["error"] = rpcErr
		} else {
			resp["result"] = result
		}
		json.NewEncoder(w).Encode(resp)
	})
}

// ---------------------------------------------------------------------------
// JSON-RPC wire types
// ---------------------------------------------------------------------------

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	ID      json.RawMessage `json:"id"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const (
	errCodeParse          = -32700
	errCodeMethodNotFound = -32601
	errCodeInvalidParams  = -32602
	errCodeInternal       = -32603
)

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"jsonrpc": "2.0",
		"error":   &rpcError{Code: code, Message: message},
		"id":      id,
	})
}

// ---------------------------------------------------------------------------
// Dispatch
// ---------------------------------------------------------------------------

func (s *Server) dispatch(method string, params json.RawMessage) (interface{}, *rpcError) {
	switch method {
	case "getVersionInfo":
		return s.getVersionInfo()
	case "getSettings":
		return s.getSettings()
	case "issueStreamKey":
		return s.issueStreamKey()
	case "broadcastChannel":
		return s.broadcastChannel(params)
	case "getChannels":
		return s.getChannels()
	case "getChannelInfo":
		return s.withChannel(params, s.getChannelInfo)
	case "getChannelStatus":
		return s.withChannel(params, s.getChannelStatus)
	case "setChannelInfo":
		return s.setChannelInfo(params)
	case "stopChannel":
		return s.withChannel(params, s.stopChannel)
	case "bumpChannel":
		return s.withChannel(params, s.bumpChannel)
	case "getChannelConnections":
		return s.withChannel(params, s.getChannelConnections)
	case "stopChannelConnection":
		return s.stopChannelConnection(params)
	case "getYellowPages":
		return s.getYellowPages()
	case "getChannelRelayTree":
		return s.withChannel(params, s.getChannelRelayTree)
	default:
		return nil, &rpcError{Code: errCodeMethodNotFound, Message: fmt.Sprintf("method not found: %s", method)}
	}
}

// withChannel parses the first positional param as a channel ID, looks up the
// active channel in the manager, then calls fn with it.
func (s *Server) withChannel(params json.RawMessage, fn func(*channel.Channel) (interface{}, *rpcError)) (interface{}, *rpcError) {
	var args []json.RawMessage
	if err := json.Unmarshal(params, &args); err != nil || len(args) == 0 {
		return nil, &rpcError{Code: errCodeInvalidParams, Message: "channelId required"}
	}
	var chanIDStr string
	if err := json.Unmarshal(args[0], &chanIDStr); err != nil {
		return nil, &rpcError{Code: errCodeInvalidParams, Message: "invalid channelId"}
	}
	ch, ok := s.lookupChannel(chanIDStr)
	if !ok {
		return nil, &rpcError{Code: errCodeInternal, Message: "channel not found"}
	}
	return fn(ch)
}

func (s *Server) lookupChannel(chanIDStr string) (*channel.Channel, bool) {
	b, err := hex.DecodeString(chanIDStr)
	if err != nil || len(b) != 16 {
		return nil, false
	}
	var id pcp.GnuID
	copy(id[:], b)
	return s.mgr.GetByID(id)
}

func gnuIDString(id pcp.GnuID) string {
	return hex.EncodeToString(id[:])
}

// ---------------------------------------------------------------------------
// Method: getVersionInfo / getSettings
// ---------------------------------------------------------------------------

func (s *Server) getVersionInfo() (interface{}, *rpcError) {
	return map[string]string{"agentName": version.AgentName}, nil
}

func (s *Server) getSettings() (interface{}, *rpcError) {
	return map[string]int{
		"serverPort": s.cfg.PeercastPort,
		"rtmpPort":   s.cfg.RTMPPort,
	}, nil
}

// ---------------------------------------------------------------------------
// Method: issueStreamKey
// ---------------------------------------------------------------------------

func (s *Server) issueStreamKey() (interface{}, *rpcError) {
	key := s.mgr.IssueStreamKey()
	return map[string]string{"streamKey": key}, nil
}

// ---------------------------------------------------------------------------
// Method: broadcastChannel
// ---------------------------------------------------------------------------

type broadcastChannelParam struct {
	SourceURI string      `json:"sourceUri"`
	Info      chanInfoArg `json:"info"`
	Track     trackInfoArg `json:"track"`
}

type chanInfoArg struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Genre   string `json:"genre"`
	Desc    string `json:"desc"`
	Comment string `json:"comment"`
	Bitrate uint32 `json:"bitrate"`
}

type trackInfoArg struct {
	Title   string `json:"title"`
	Creator string `json:"creator"`
	URL     string `json:"url"`
	Album   string `json:"album"`
}

func (s *Server) broadcastChannel(params json.RawMessage) (interface{}, *rpcError) {
	var args []broadcastChannelParam
	if err := json.Unmarshal(params, &args); err != nil || len(args) == 0 {
		return nil, &rpcError{Code: errCodeInvalidParams, Message: "expected [{sourceUri, info, track}]"}
	}
	p := args[0]

	if p.Info.Name == "" {
		return nil, &rpcError{Code: errCodeInvalidParams, Message: "channel name must not be empty"}
	}

	streamKey, err := extractStreamKey(p.SourceURI)
	if err != nil {
		return nil, &rpcError{Code: errCodeInvalidParams, Message: err.Error()}
	}

	info := channel.ChannelInfo{
		Name:     p.Info.Name,
		URL:      p.Info.URL,
		Genre:    p.Info.Genre,
		Desc:     p.Info.Desc,
		Comment:  p.Info.Comment,
		Bitrate:  p.Info.Bitrate,
		Type:     "FLV",
		MIMEType: "video/x-flv",
		Ext:      ".flv",
	}
	track := channel.TrackInfo{
		Title:   p.Track.Title,
		Creator: p.Track.Creator,
		URL:     p.Track.URL,
		Album:   p.Track.Album,
	}

	ch, err := s.mgr.Broadcast(streamKey, info, track)
	if err != nil {
		return nil, &rpcError{Code: errCodeInvalidParams, Message: err.Error()}
	}

	return map[string]string{"channelId": gnuIDString(ch.ID)}, nil
}

// extractStreamKey parses the stream key from an RTMP source URI of the form
// rtmp://host:port/live/<streamKey>.
func extractStreamKey(sourceURI string) (string, error) {
	const prefix = "/live/"
	idx := strings.LastIndex(sourceURI, prefix)
	if idx < 0 {
		return "", fmt.Errorf("sourceUri must contain /live/<streamKey>")
	}
	key := sourceURI[idx+len(prefix):]
	if key == "" {
		return "", fmt.Errorf("stream key is empty in sourceUri")
	}
	return key, nil
}

// ---------------------------------------------------------------------------
// Response types
// ---------------------------------------------------------------------------

type chanInfoResult struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Desc    string `json:"desc"`
	Comment string `json:"comment"`
	Genre   string `json:"genre"`
	Type    string `json:"type"`
	Bitrate uint32 `json:"bitrate"`
}

type trackInfoResult struct {
	Title   string `json:"title"`
	Creator string `json:"creator"`
	URL     string `json:"url"`
	Album   string `json:"album"`
}

type ypRef struct {
	YellowPageID int    `json:"yellowPageId"`
	Name         string `json:"name"`
}

type chanStatusResult struct {
	Status         string `json:"status"`
	Source         string `json:"source"`
	TotalDirects   int    `json:"totalDirects"`
	TotalRelays    int    `json:"totalRelays"`
	IsBroadcasting bool   `json:"isBroadcasting"`
	IsRelayFull    bool   `json:"isRelayFull"`
	IsDirectFull   bool   `json:"isDirectFull"`
	IsReceiving    bool   `json:"isReceiving"`
}

// ---------------------------------------------------------------------------
// Builders
// ---------------------------------------------------------------------------

func (s *Server) buildChanInfo(ch *channel.Channel) chanInfoResult {
	info := ch.Info()
	return chanInfoResult{
		Name:    info.Name,
		URL:     info.URL,
		Desc:    info.Desc,
		Comment: info.Comment,
		Genre:   info.Genre,
		Type:    info.Type,
		Bitrate: info.Bitrate,
	}
}

func (s *Server) buildTrackInfo(ch *channel.Channel) trackInfoResult {
	track := ch.Track()
	return trackInfoResult{
		Title:   track.Title,
		Creator: track.Creator,
		URL:     track.URL,
		Album:   track.Album,
	}
}

func (s *Server) buildYPRefs() []ypRef {
	refs := make([]ypRef, len(s.cfg.YPs))
	for i, entry := range s.cfg.YPs {
		refs[i] = ypRef{YellowPageID: i, Name: entry.Name}
	}
	return refs
}

func (s *Server) buildStatus(ch *channel.Channel) chanStatusResult {
	source := fmt.Sprintf("rtmp://127.0.0.1:%d/live", s.cfg.RTMPPort)
	if key, ok := s.mgr.StreamKeyByID(ch.ID); ok {
		source = fmt.Sprintf("rtmp://127.0.0.1:%d/live/%s", s.cfg.RTMPPort, key)
	}
	receiving := ch.Buffer.HasData()
	status := "Idle"
	if receiving {
		status = "Receiving"
	}
	return chanStatusResult{
		Status:         status,
		Source:         source,
		TotalDirects:   ch.NumListeners(),
		TotalRelays:    ch.NumRelays(),
		IsBroadcasting: receiving,
		IsRelayFull:    ch.IsRelayFull(s.cfg.MaxRelays),
		IsDirectFull:   ch.IsDirectFull(s.cfg.MaxListeners),
		IsReceiving:    receiving,
	}
}

// ---------------------------------------------------------------------------
// Method: channel queries
// ---------------------------------------------------------------------------

func (s *Server) getChannels() (interface{}, *rpcError) {
	type chanEntry struct {
		ChannelID   string           `json:"channelId"`
		Status      chanStatusResult `json:"status"`
		Info        chanInfoResult   `json:"info"`
		Track       trackInfoResult  `json:"track"`
		YellowPages []ypRef          `json:"yellowPages"`
	}
	channels := s.mgr.List()
	entries := make([]chanEntry, len(channels))
	for i, ch := range channels {
		entries[i] = chanEntry{
			ChannelID:   gnuIDString(ch.ID),
			Status:      s.buildStatus(ch),
			Info:        s.buildChanInfo(ch),
			Track:       s.buildTrackInfo(ch),
			YellowPages: s.buildYPRefs(),
		}
	}
	return entries, nil
}

func (s *Server) getChannelInfo(ch *channel.Channel) (interface{}, *rpcError) {
	type result struct {
		Info        chanInfoResult  `json:"info"`
		Track       trackInfoResult `json:"track"`
		YellowPages []ypRef         `json:"yellowPages"`
	}
	return result{
		Info:        s.buildChanInfo(ch),
		Track:       s.buildTrackInfo(ch),
		YellowPages: s.buildYPRefs(),
	}, nil
}

func (s *Server) getChannelStatus(ch *channel.Channel) (interface{}, *rpcError) {
	return s.buildStatus(ch), nil
}

func (s *Server) setChannelInfo(params json.RawMessage) (interface{}, *rpcError) {
	var args []json.RawMessage
	if err := json.Unmarshal(params, &args); err != nil || len(args) < 3 {
		return nil, &rpcError{Code: errCodeInvalidParams, Message: "expected [channelId, info, track]"}
	}
	var chanIDStr string
	if err := json.Unmarshal(args[0], &chanIDStr); err != nil {
		return nil, &rpcError{Code: errCodeInvalidParams, Message: "invalid channelId"}
	}
	ch, ok := s.lookupChannel(chanIDStr)
	if !ok {
		return nil, &rpcError{Code: errCodeInternal, Message: "channel not found"}
	}

	var infoArg chanInfoArg
	if err := json.Unmarshal(args[1], &infoArg); err != nil {
		return nil, &rpcError{Code: errCodeInvalidParams, Message: "invalid info"}
	}
	var trackArg trackInfoArg
	if err := json.Unmarshal(args[2], &trackArg); err != nil {
		return nil, &rpcError{Code: errCodeInvalidParams, Message: "invalid track"}
	}

	cur := ch.Info()
	cur.Name = infoArg.Name
	cur.URL = infoArg.URL
	cur.Desc = infoArg.Desc
	cur.Comment = infoArg.Comment
	cur.Genre = infoArg.Genre
	if infoArg.Bitrate > 0 {
		cur.Bitrate = infoArg.Bitrate
	}
	ch.SetInfo(cur)

	ch.SetTrack(channel.TrackInfo{
		Title:   trackArg.Title,
		Creator: trackArg.Creator,
		URL:     trackArg.URL,
		Album:   trackArg.Album,
	})
	return nil, nil
}

func (s *Server) stopChannel(ch *channel.Channel) (interface{}, *rpcError) {
	s.mgr.Stop(ch.ID)
	return nil, nil
}

func (s *Server) bumpChannel(_ *channel.Channel) (interface{}, *rpcError) {
	if s.ypClient != nil {
		s.ypClient.Bump()
	}
	return nil, nil
}

// ---------------------------------------------------------------------------
// Method: connection management
// ---------------------------------------------------------------------------

type connEntry struct {
	ConnectionID   int     `json:"connectionId"`
	Type           string  `json:"type"`
	Status         string  `json:"status"`
	SendRate       int64   `json:"sendRate"`
	RecvRate       int64   `json:"recvRate"`
	ProtocolName   string  `json:"protocolName"`
	RemoteEndPoint *string `json:"remoteEndPoint"`
}

func (s *Server) getChannelConnections(ch *channel.Channel) (interface{}, *rpcError) {
	sourceAddr := fmt.Sprintf("127.0.0.1:%d", s.cfg.RTMPPort)
	sourceStatus := "Idle"
	if ch.Buffer.HasData() {
		sourceStatus = "Receiving"
	}
	result := []connEntry{
		{
			ConnectionID:   -1,
			Type:           "source",
			Status:         sourceStatus,
			SendRate:       0,
			RecvRate:       0,
			ProtocolName:   "RTMP",
			RemoteEndPoint: &sourceAddr,
		},
	}

	for _, ci := range ch.Connections() {
		typ := "relay"
		proto := "PCP"
		if ci.Type == channel.OutputStreamHTTP {
			typ = "direct"
			proto = "HTTP"
		}
		addr := ci.RemoteAddr
		result = append(result, connEntry{
			ConnectionID:   ci.ID,
			Type:           typ,
			Status:         "Connected",
			SendRate:       ci.SendRate,
			RecvRate:       0,
			ProtocolName:   proto,
			RemoteEndPoint: &addr,
		})
	}
	return result, nil
}

func (s *Server) stopChannelConnection(params json.RawMessage) (interface{}, *rpcError) {
	var args []json.RawMessage
	if err := json.Unmarshal(params, &args); err != nil || len(args) < 2 {
		return nil, &rpcError{Code: errCodeInvalidParams, Message: "expected [channelId, connectionId]"}
	}
	var chanIDStr string
	if err := json.Unmarshal(args[0], &chanIDStr); err != nil {
		return nil, &rpcError{Code: errCodeInvalidParams, Message: "invalid channelId"}
	}
	ch, ok := s.lookupChannel(chanIDStr)
	if !ok {
		return nil, &rpcError{Code: errCodeInternal, Message: "channel not found"}
	}

	var connID int
	if err := json.Unmarshal(args[1], &connID); err != nil {
		return nil, &rpcError{Code: errCodeInvalidParams, Message: "invalid connectionId"}
	}

	// Only relay (PCP) connections can be stopped.
	for _, ci := range ch.Connections() {
		if ci.ID == connID {
			if ci.Type != channel.OutputStreamPCP {
				return false, nil
			}
			return ch.CloseConnection(connID), nil
		}
	}
	return false, nil
}

// ---------------------------------------------------------------------------
// Method: getYellowPages
// ---------------------------------------------------------------------------

type ypEntry struct {
	YellowPageID int    `json:"yellowPageId"`
	Name         string `json:"name"`
	URI          string `json:"uri"`
	AnnounceURI  string `json:"announceUri"`
	ChannelCount int    `json:"channelCount"`
}

func (s *Server) getYellowPages() (interface{}, *rpcError) {
	result := make([]ypEntry, len(s.cfg.YPs))
	for i, entry := range s.cfg.YPs {
		uri := entry.Addr
		if !strings.Contains(uri, "://") {
			uri = "pcp://" + uri + "/"
		}
		count := 0
		if s.ypClient != nil {
			count = len(s.mgr.List())
		}
		result[i] = ypEntry{
			YellowPageID: i,
			Name:         entry.Name,
			URI:          uri,
			AnnounceURI:  uri,
			ChannelCount: count,
		}
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// Method: getChannelRelayTree
// ---------------------------------------------------------------------------

type relayTreeNode struct {
	SessionID     string          `json:"sessionId"`
	Address       string          `json:"address"`
	Port          int             `json:"port"`
	IsFirewalled  bool            `json:"isFirewalled"`
	LocalRelays   int             `json:"localRelays"`
	LocalDirects  int             `json:"localDirects"`
	IsTracker     bool            `json:"isTracker"`
	IsRelayFull   bool            `json:"isRelayFull"`
	IsDirectFull  bool            `json:"isDirectFull"`
	IsReceiving   bool            `json:"isReceiving"`
	IsControlFull bool            `json:"isControlFull"`
	Version       int             `json:"version"`
	VersionString string          `json:"versionString"`
	Children      []relayTreeNode `json:"children"`
}

func (s *Server) getChannelRelayTree(ch *channel.Channel) (interface{}, *rpcError) {
	node := relayTreeNode{
		SessionID:     gnuIDString(s.sessionID),
		Address:       "",
		Port:          s.cfg.PeercastPort,
		IsFirewalled:  false,
		LocalRelays:   ch.NumRelays(),
		LocalDirects:  ch.NumListeners(),
		IsTracker:     true,
		IsRelayFull:   ch.IsRelayFull(s.cfg.MaxRelays),
		IsDirectFull:  ch.IsDirectFull(s.cfg.MaxListeners),
		IsReceiving:   ch.Buffer.HasData(),
		IsControlFull: false,
		Version:       version.PCPVersion,
		VersionString: version.AgentName,
		Children:      []relayTreeNode{},
	}
	return []relayTreeNode{node}, nil
}
