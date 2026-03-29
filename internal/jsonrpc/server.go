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
	"github.com/titagaki/peercast-mm/internal/yp"
)

// Server handles JSON-RPC 2.0 requests at POST /api/1.
type Server struct {
	sessionID pcp.GnuID
	ch        *channel.Channel
	cfg       *config.Config
	ypClient  *yp.Client // may be nil
}

// New creates a new JSON-RPC Server.
func New(sessionID pcp.GnuID, ch *channel.Channel, cfg *config.Config, ypClient *yp.Client) *Server {
	return &Server{
		sessionID: sessionID,
		ch:        ch,
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
	case "getChannels":
		return s.getChannels()
	case "getChannelInfo":
		return s.withChannelID(params, s.getChannelInfo)
	case "getChannelStatus":
		return s.withChannelID(params, s.getChannelStatus)
	case "setChannelInfo":
		return s.setChannelInfo(params)
	case "stopChannel":
		return s.withChannelID(params, s.stopChannel)
	case "bumpChannel":
		return s.withChannelID(params, s.bumpChannel)
	case "getChannelConnections":
		return s.withChannelID(params, s.getChannelConnections)
	case "stopChannelConnection":
		return s.stopChannelConnection(params)
	case "getYellowPages":
		return s.getYellowPages()
	case "getChannelRelayTree":
		return s.withChannelID(params, s.getChannelRelayTree)
	default:
		return nil, &rpcError{Code: errCodeMethodNotFound, Message: fmt.Sprintf("method not found: %s", method)}
	}
}

// withChannelID parses the first positional param as a channel ID and verifies
// it matches the active channel, then calls fn.
func (s *Server) withChannelID(params json.RawMessage, fn func() (interface{}, *rpcError)) (interface{}, *rpcError) {
	var args []json.RawMessage
	if err := json.Unmarshal(params, &args); err != nil || len(args) == 0 {
		return nil, &rpcError{Code: errCodeInvalidParams, Message: "channelId required"}
	}
	var chanIDStr string
	if err := json.Unmarshal(args[0], &chanIDStr); err != nil {
		return nil, &rpcError{Code: errCodeInvalidParams, Message: "invalid channelId"}
	}
	if !s.matchChannelID(chanIDStr) {
		return nil, &rpcError{Code: errCodeInternal, Message: "channel not found"}
	}
	return fn()
}

func (s *Server) matchChannelID(chanIDStr string) bool {
	expected := hex.EncodeToString(s.ch.ID[:])
	return strings.EqualFold(chanIDStr, expected)
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

func (s *Server) buildChanInfo() chanInfoResult {
	info := s.ch.Info()
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

func (s *Server) buildTrackInfo() trackInfoResult {
	track := s.ch.Track()
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

func (s *Server) buildStatus() chanStatusResult {
	return chanStatusResult{
		Status:         "Receiving",
		Source:         fmt.Sprintf("rtmp://127.0.0.1:%d/live", s.cfg.RTMPPort),
		TotalDirects:   s.ch.NumListeners(),
		TotalRelays:    s.ch.NumRelays(),
		IsBroadcasting: true,
		IsRelayFull:    s.ch.IsRelayFull(s.cfg.MaxRelays),
		IsDirectFull:   s.ch.IsDirectFull(s.cfg.MaxListeners),
		IsReceiving:    s.ch.Buffer.HasData(),
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
	entry := chanEntry{
		ChannelID:   gnuIDString(s.ch.ID),
		Status:      s.buildStatus(),
		Info:        s.buildChanInfo(),
		Track:       s.buildTrackInfo(),
		YellowPages: s.buildYPRefs(),
	}
	return []chanEntry{entry}, nil
}

func (s *Server) getChannelInfo() (interface{}, *rpcError) {
	type result struct {
		Info        chanInfoResult  `json:"info"`
		Track       trackInfoResult `json:"track"`
		YellowPages []ypRef         `json:"yellowPages"`
	}
	return result{
		Info:        s.buildChanInfo(),
		Track:       s.buildTrackInfo(),
		YellowPages: s.buildYPRefs(),
	}, nil
}

func (s *Server) getChannelStatus() (interface{}, *rpcError) {
	return s.buildStatus(), nil
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
	if !s.matchChannelID(chanIDStr) {
		return nil, &rpcError{Code: errCodeInternal, Message: "channel not found"}
	}

	var infoArg chanInfoResult
	if err := json.Unmarshal(args[1], &infoArg); err != nil {
		return nil, &rpcError{Code: errCodeInvalidParams, Message: "invalid info"}
	}
	var trackArg trackInfoResult
	if err := json.Unmarshal(args[2], &trackArg); err != nil {
		return nil, &rpcError{Code: errCodeInvalidParams, Message: "invalid track"}
	}

	cur := s.ch.Info()
	cur.Name = infoArg.Name
	cur.URL = infoArg.URL
	cur.Desc = infoArg.Desc
	cur.Comment = infoArg.Comment
	cur.Genre = infoArg.Genre
	if infoArg.Bitrate > 0 {
		cur.Bitrate = infoArg.Bitrate
	}
	s.ch.SetInfo(cur)

	s.ch.SetTrack(channel.TrackInfo{
		Title:   trackArg.Title,
		Creator: trackArg.Creator,
		URL:     trackArg.URL,
		Album:   trackArg.Album,
	})
	return nil, nil
}

func (s *Server) stopChannel() (interface{}, *rpcError) {
	s.ch.CloseAll()
	return nil, nil
}

func (s *Server) bumpChannel() (interface{}, *rpcError) {
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

func (s *Server) getChannelConnections() (interface{}, *rpcError) {
	sourceAddr := fmt.Sprintf("127.0.0.1:%d", s.cfg.RTMPPort)
	result := []connEntry{
		{
			ConnectionID:   -1,
			Type:           "source",
			Status:         "Receiving",
			SendRate:       0,
			RecvRate:       0,
			ProtocolName:   "RTMP",
			RemoteEndPoint: &sourceAddr,
		},
	}

	for _, ci := range s.ch.Connections() {
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
	if !s.matchChannelID(chanIDStr) {
		return nil, &rpcError{Code: errCodeInternal, Message: "channel not found"}
	}

	var connID int
	if err := json.Unmarshal(args[1], &connID); err != nil {
		return nil, &rpcError{Code: errCodeInvalidParams, Message: "invalid connectionId"}
	}

	// Only relay (PCP) connections can be stopped.
	for _, ci := range s.ch.Connections() {
		if ci.ID == connID {
			if ci.Type != channel.OutputStreamPCP {
				return false, nil
			}
			return s.ch.CloseConnection(connID), nil
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
			count = 1
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

func (s *Server) getChannelRelayTree() (interface{}, *rpcError) {
	node := relayTreeNode{
		SessionID:     gnuIDString(s.sessionID),
		Address:       "",
		Port:          s.cfg.PeercastPort,
		IsFirewalled:  false,
		LocalRelays:   s.ch.NumRelays(),
		LocalDirects:  s.ch.NumListeners(),
		IsTracker:     true,
		IsRelayFull:   s.ch.IsRelayFull(s.cfg.MaxRelays),
		IsDirectFull:  s.ch.IsDirectFull(s.cfg.MaxListeners),
		IsReceiving:   s.ch.Buffer.HasData(),
		IsControlFull: false,
		Version:       version.PCPVersion,
		VersionString: version.AgentName,
		Children:      []relayTreeNode{},
	}
	return []relayTreeNode{node}, nil
}
