package relay

import (
	"net"
	"testing"
	"time"

	"github.com/titagaki/peercast-pcp/pcp"
)

func TestSourceNodeList_AddAndList(t *testing.T) {
	l := NewSourceNodeList()

	id1 := pcp.GnuID{1}
	id2 := pcp.GnuID{2}

	l.Add(SourceNode{SessionID: id1, GlobalAddr: "1.2.3.4:7144"})
	l.Add(SourceNode{SessionID: id2, GlobalAddr: "5.6.7.8:7144"})

	nodes := l.List()
	if len(nodes) != 2 {
		t.Fatalf("got %d nodes, want 2", len(nodes))
	}
}

func TestSourceNodeList_Dedup(t *testing.T) {
	l := NewSourceNodeList()
	id := pcp.GnuID{1}

	l.Add(SourceNode{SessionID: id, GlobalAddr: "1.2.3.4:7144", RelayCount: 1})
	l.Add(SourceNode{SessionID: id, GlobalAddr: "1.2.3.4:7144", RelayCount: 5})

	nodes := l.List()
	if len(nodes) != 1 {
		t.Fatalf("got %d nodes, want 1", len(nodes))
	}
	if nodes[0].RelayCount != 5 {
		t.Errorf("RelayCount = %d, want 5 (should be updated)", nodes[0].RelayCount)
	}
}

func TestSourceNodeList_Expiry(t *testing.T) {
	l := NewSourceNodeList()
	id := pcp.GnuID{1}

	l.Add(SourceNode{SessionID: id, GlobalAddr: "1.2.3.4:7144"})

	// Manually set the UpdatedAt to be old.
	l.mu.Lock()
	l.nodes[id].UpdatedAt = time.Now().Add(-4 * time.Minute)
	l.mu.Unlock()

	// Adding a new node triggers pruning.
	l.Add(SourceNode{SessionID: pcp.GnuID{2}, GlobalAddr: "5.6.7.8:7144"})

	nodes := l.List()
	if len(nodes) != 1 {
		t.Fatalf("got %d nodes, want 1 (expired node should be pruned)", len(nodes))
	}
	if nodes[0].GlobalAddr != "5.6.7.8:7144" {
		t.Errorf("remaining node = %s, want 5.6.7.8:7144", nodes[0].GlobalAddr)
	}
}

func TestParseSourceNode_Basic(t *testing.T) {
	// Build a HOST atom with dual IP/port, flags, and metadata.
	globalIP, _ := pcp.IPv4ToUint32(net.IPv4(1, 2, 3, 4))
	localIP, _ := pcp.IPv4ToUint32(net.IPv4(192, 168, 1, 100))
	sessionID := pcp.GnuID{0xAA, 0xBB}

	atom := pcp.NewParentAtom(pcp.PCPHost,
		pcp.NewIDAtom(pcp.PCPHostID, sessionID),
		pcp.NewIntAtom(pcp.PCPHostIP, globalIP),
		pcp.NewShortAtom(pcp.PCPHostPort, 7144),
		pcp.NewIntAtom(pcp.PCPHostIP, localIP),
		pcp.NewShortAtom(pcp.PCPHostPort, 7144),
		pcp.NewByteAtom(pcp.PCPHostFlags1, pcp.PCPHostFlags1Relay|pcp.PCPHostFlags1Recv),
		pcp.NewIntAtom(pcp.PCPHostNumRelays, 3),
		pcp.NewIntAtom(pcp.PCPHostUphostHops, 2),
	)

	node, ok := parseSourceNode(atom)
	if !ok {
		t.Fatal("parseSourceNode returned false")
	}
	if node.SessionID != sessionID {
		t.Errorf("SessionID = %v, want %v", node.SessionID, sessionID)
	}
	if node.GlobalAddr != "1.2.3.4:7144" {
		t.Errorf("GlobalAddr = %s, want 1.2.3.4:7144", node.GlobalAddr)
	}
	if node.LocalAddr != "192.168.1.100:7144" {
		t.Errorf("LocalAddr = %s, want 192.168.1.100:7144", node.LocalAddr)
	}
	if node.IsFirewalled {
		t.Error("IsFirewalled should be false")
	}
	if node.IsRelayFull {
		t.Error("IsRelayFull should be false (relay flag is set)")
	}
	if !node.IsReceiving {
		t.Error("IsReceiving should be true")
	}
	if node.RelayCount != 3 {
		t.Errorf("RelayCount = %d, want 3", node.RelayCount)
	}
	if node.Hops != 2 {
		t.Errorf("Hops = %d, want 2", node.Hops)
	}
}

func TestParseSourceNode_Firewalled(t *testing.T) {
	globalIP, _ := pcp.IPv4ToUint32(net.IPv4(1, 2, 3, 4))
	atom := pcp.NewParentAtom(pcp.PCPHost,
		pcp.NewIntAtom(pcp.PCPHostIP, globalIP),
		pcp.NewShortAtom(pcp.PCPHostPort, 7144),
		pcp.NewByteAtom(pcp.PCPHostFlags1, pcp.PCPHostFlags1Push),
	)

	node, ok := parseSourceNode(atom)
	if !ok {
		t.Fatal("parseSourceNode returned false")
	}
	if !node.IsFirewalled {
		t.Error("IsFirewalled should be true")
	}
}

func TestParseSourceNode_NoAddress(t *testing.T) {
	atom := pcp.NewParentAtom(pcp.PCPHost,
		pcp.NewByteAtom(pcp.PCPHostFlags1, pcp.PCPHostFlags1Relay),
	)

	_, ok := parseSourceNode(atom)
	if ok {
		t.Error("parseSourceNode should return false for node with no address")
	}
}
