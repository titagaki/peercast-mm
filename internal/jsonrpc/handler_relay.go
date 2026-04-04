package jsonrpc

import (
	"net"
	"strconv"

	"github.com/titagaki/peercast-mi/internal/channel"
	"github.com/titagaki/peercast-mi/internal/version"
)

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
		IsReceiving:   ch.HasData(),
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
