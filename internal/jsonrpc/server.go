package jsonrpc

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	"github.com/titagaki/peercast-pcp/pcp"

	"github.com/titagaki/peercast-mi/internal/channel"
	"github.com/titagaki/peercast-mi/internal/config"
)

// YPBumper is the subset of yp.Client that the JSON-RPC server needs.
type YPBumper interface {
	Bump()
}

// ChannelManager is the subset of channel.Manager that the JSON-RPC server needs.
type ChannelManager interface {
	IssueStreamKey(accountName, streamKey string) error
	RevokeStreamKey(accountName string) bool
	Broadcast(streamKey string, info channel.ChannelInfo, track channel.TrackInfo) (*channel.Channel, error)
	Stop(channelID pcp.GnuID) bool
	GetByID(channelID pcp.GnuID) (*channel.Channel, bool)
	StreamKeyByID(channelID pcp.GnuID) (string, bool)
	List() []*channel.Channel
}

// Server handles JSON-RPC 2.0 requests at POST /api/1.
type Server struct {
	sessionID pcp.GnuID
	mgr       ChannelManager
	cfg       *config.Config
	ypClient  YPBumper // may be nil
}

// New creates a new JSON-RPC Server.
func New(sessionID pcp.GnuID, mgr ChannelManager, cfg *config.Config, ypClient YPBumper) *Server {
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
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if !isLocalhost(r.RemoteAddr) {
			if !s.checkBasicAuth(r) {
				w.Header().Set("WWW-Authenticate", `Basic realm="peercast-mi"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}

		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeRPCError(w, nil, errCodeParse, "parse error")
			return
		}

		slog.Debug("jsonrpc: request", "remote", r.RemoteAddr, "method", req.Method)
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

// checkBasicAuth returns true if the request carries valid Basic auth credentials
// as configured in cfg. Returns false if credentials are not configured or do not match.
func (s *Server) checkBasicAuth(r *http.Request) bool {
	if s.cfg.AdminUser == "" || s.cfg.AdminPass == "" {
		return false
	}
	user, pass, ok := r.BasicAuth()
	return ok && user == s.cfg.AdminUser && pass == s.cfg.AdminPass
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

// isLocalhost reports whether remoteAddr (in "host:port" form) is a
// loopback address.
func isLocalhost(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

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
		return s.issueStreamKey(params)
	case "revokeStreamKey":
		return s.revokeStreamKey(params)
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
