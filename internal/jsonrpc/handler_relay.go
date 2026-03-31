package jsonrpc

import (
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net"
	"strconv"

	"github.com/titagaki/peercast-pcp/pcp"

	"github.com/titagaki/peercast-mm/internal/channel"
	"github.com/titagaki/peercast-mm/internal/relay"
	"github.com/titagaki/peercast-mm/internal/version"
)

// ---------------------------------------------------------------------------
// relayChannel
// ---------------------------------------------------------------------------

type relayChannelParam struct {
	UpstreamAddr string `json:"upstreamAddr"` // "host:port"
	ChannelID    string `json:"channelId"`    // 32-char hex
}

func (s *Server) relayChannel(params json.RawMessage) (interface{}, *rpcError) {
	var args []relayChannelParam
	if err := json.Unmarshal(params, &args); err != nil || len(args) == 0 {
		return nil, &rpcError{Code: errCodeInvalidParams, Message: "expected [{upstreamAddr, channelId}]"}
	}
	p := args[0]

	upstreamAddr := p.UpstreamAddr
	if upstreamAddr == "" {
		// Auto-discover: use the first configured YP as the upstream source.
		if len(s.cfg.YPs) == 0 {
			return nil, &rpcError{Code: errCodeInvalidParams, Message: "upstreamAddr is required when no YP is configured"}
		}
		hp, err := s.cfg.YPs[0].HostPort()
		if err != nil {
			return nil, &rpcError{Code: errCodeInternal, Message: "failed to resolve YP address"}
		}
		upstreamAddr = hp
		slog.Info("relay: auto-discovered upstream from YP", "addr", upstreamAddr)
	}

	b, err := hex.DecodeString(p.ChannelID)
	if err != nil || len(b) != 16 {
		return nil, &rpcError{Code: errCodeInvalidParams, Message: "channelId must be a 32-char hex string"}
	}
	var chanID pcp.GnuID
	copy(chanID[:], b)

	if _, ok := s.mgr.GetByID(chanID); ok {
		return nil, &rpcError{Code: errCodeInvalidParams, Message: "channel already active"}
	}

	ch := channel.New(chanID, pcp.GnuID{}, 0)
	ch.SetSource(upstreamAddr)
	ch.SetUpstreamAddr(upstreamAddr)
	client := relay.New(upstreamAddr, chanID, s.sessionID, ch)
	s.mgr.AddRelayChannel(ch, client)
	go client.Run()

	slog.Info("relay: started", "addr", upstreamAddr, "channel", p.ChannelID)
	return map[string]string{"channelId": gnuIDString(chanID)}, nil
}

// ---------------------------------------------------------------------------
// getChannelRelayTree
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
	thisNode := relayTreeNode{
		SessionID:     gnuIDString(s.sessionID),
		Address:       "",
		Port:          s.cfg.PeercastPort,
		IsFirewalled:  false,
		LocalRelays:   ch.NumRelays(),
		LocalDirects:  ch.NumListeners(),
		IsTracker:     ch.IsBroadcasting(),
		IsRelayFull:   ch.IsRelayFull(s.cfg.MaxRelays),
		IsDirectFull:  ch.IsDirectFull(s.cfg.MaxListeners),
		IsReceiving:   ch.Buffer.HasData(),
		IsControlFull: false,
		Version:       version.PCPVersion,
		VersionString: version.AgentName,
		Children:      []relayTreeNode{},
	}

	if upstream := ch.UpstreamAddr(); upstream != "" {
		host, portStr, _ := net.SplitHostPort(upstream)
		port, _ := strconv.Atoi(portStr)
		upstreamNode := relayTreeNode{
			SessionID:     "",
			Address:       host,
			Port:          port,
			IsFirewalled:  false,
			LocalRelays:   0,
			LocalDirects:  0,
			IsTracker:     false,
			IsRelayFull:   false,
			IsDirectFull:  false,
			IsReceiving:   true,
			IsControlFull: false,
			Version:       0,
			VersionString: "",
			Children:      []relayTreeNode{thisNode},
		}
		return []relayTreeNode{upstreamNode}, nil
	}

	return []relayTreeNode{thisNode}, nil
}
