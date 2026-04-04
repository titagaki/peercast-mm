package relay

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/titagaki/peercast-pcp/pcp"
)

const nodeExpiry = 3 * time.Minute

// SourceNode represents a candidate upstream host learned from PCP HOST atoms.
type SourceNode struct {
	SessionID    pcp.GnuID
	GlobalAddr   string // "ip:port" external address
	LocalAddr    string // "ip:port" internal address (may be empty)
	IsFirewalled bool
	IsRelayFull  bool
	IsReceiving  bool
	IsTracker    bool
	Hops         uint32
	RelayCount   uint32
	UpdatedAt    time.Time
}

// SourceNodeList is a thread-safe collection of source nodes, keyed by SessionID.
type SourceNodeList struct {
	mu    sync.Mutex
	nodes map[pcp.GnuID]*SourceNode
}

func NewSourceNodeList() *SourceNodeList {
	return &SourceNodeList{nodes: make(map[pcp.GnuID]*SourceNode)}
}

// Add upserts a node by SessionID and prunes expired entries.
func (l *SourceNodeList) Add(node SourceNode) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	node.UpdatedAt = now
	l.nodes[node.SessionID] = &node

	for id, n := range l.nodes {
		if now.Sub(n.UpdatedAt) > nodeExpiry {
			delete(l.nodes, id)
		}
	}
}

// List returns a snapshot of non-expired nodes.
func (l *SourceNodeList) List() []SourceNode {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	var result []SourceNode
	for _, n := range l.nodes {
		if now.Sub(n.UpdatedAt) <= nodeExpiry {
			result = append(result, *n)
		}
	}
	return result
}

// parseSourceNode extracts a SourceNode from a PCP HOST atom.
// A HOST atom contains two consecutive IP/port pairs: rhost[0] is the
// external (global) address, rhost[1] is the internal (local) address.
func parseSourceNode(atom *pcp.Atom) (SourceNode, bool) {
	var ips [2]uint32
	var ports [2]uint16
	ipIdx, portIdx := 0, 0

	var node SourceNode

	for _, child := range atom.Children() {
		switch child.Tag {
		case pcp.PCPHostID:
			if v, err := child.GetID(); err == nil {
				node.SessionID = v
			}
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
				node.IsFirewalled = v&pcp.PCPHostFlags1Push != 0
				node.IsRelayFull = v&pcp.PCPHostFlags1Relay == 0
				node.IsReceiving = v&pcp.PCPHostFlags1Recv != 0
				node.IsTracker = v&pcp.PCPHostFlags1Tracker != 0
			}
		case pcp.PCPHostNumRelays:
			if v, err := child.GetInt(); err == nil {
				node.RelayCount = v
			}
		case pcp.PCPHostUphostHops:
			if v, err := child.GetInt(); err == nil {
				node.Hops = v
			}
		}
	}

	// Build global address from first IP/port pair.
	if ports[0] != 0 {
		ip := pcp.IPv4FromUint32(ips[0])
		if !ip.IsUnspecified() && !ip.IsLoopback() {
			node.GlobalAddr = fmt.Sprintf("%s:%d", ip, ports[0])
		}
	}

	// Build local address from second IP/port pair.
	if ports[1] != 0 {
		ip := pcp.IPv4FromUint32(ips[1])
		if !ip.IsUnspecified() && !ip.IsLoopback() {
			node.LocalAddr = fmt.Sprintf("%s:%d", ip, ports[1])
		}
	}

	if node.GlobalAddr == "" && node.LocalAddr == "" {
		return node, false
	}
	return node, true
}

// isSiteLocal returns true if addr is on the same local network.
func isSiteLocal(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsPrivate()
}
