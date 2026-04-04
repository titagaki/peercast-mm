package relay

import (
	"testing"
	"time"

	"github.com/titagaki/peercast-pcp/pcp"
)

func TestSelectSourceHost_PicksBestNode(t *testing.T) {
	ignored := NewIgnoredNodeCollection()

	nodes := []SourceNode{
		{SessionID: pcp.GnuID{1}, GlobalAddr: "1.2.3.4:7144", IsReceiving: false, IsRelayFull: true, Hops: 9},
		{SessionID: pcp.GnuID{2}, GlobalAddr: "5.6.7.8:7144", IsReceiving: true, IsRelayFull: false, Hops: 1},
	}

	// Node 1 max score: rand * (0 + 0 + 100 + 0) = ~100
	// Node 2 max score: rand * (4000 + 2000 + 900 + 0) = ~6900
	// Node 2 should win overwhelmingly.
	wins := map[string]int{}
	for i := 0; i < 100; i++ {
		addr := selectSourceHost(nodes, ignored, "tracker:7144")
		wins[addr]++
	}
	if wins["5.6.7.8:7144"] < 90 {
		t.Errorf("expected node 2 to win most of the time, got %v", wins)
	}
}

func TestSelectSourceHost_SkipsIgnored(t *testing.T) {
	ignored := NewIgnoredNodeCollection()
	ignored.Add("1.2.3.4:7144")

	nodes := []SourceNode{
		{SessionID: pcp.GnuID{1}, GlobalAddr: "1.2.3.4:7144", IsReceiving: true},
		{SessionID: pcp.GnuID{2}, GlobalAddr: "5.6.7.8:7144", IsReceiving: true},
	}

	addr := selectSourceHost(nodes, ignored, "tracker:7144")
	if addr != "5.6.7.8:7144" {
		t.Errorf("got %s, want 5.6.7.8:7144 (1.2.3.4 is ignored)", addr)
	}
}

func TestSelectSourceHost_SkipsFirewalled(t *testing.T) {
	ignored := NewIgnoredNodeCollection()

	nodes := []SourceNode{
		{SessionID: pcp.GnuID{1}, GlobalAddr: "1.2.3.4:7144", IsFirewalled: true, IsReceiving: true},
	}

	addr := selectSourceHost(nodes, ignored, "tracker:7144")
	if addr != "tracker:7144" {
		t.Errorf("got %s, want tracker:7144 (firewalled node should be skipped)", addr)
	}
}

func TestSelectSourceHost_FallsBackToTracker(t *testing.T) {
	ignored := NewIgnoredNodeCollection()

	addr := selectSourceHost(nil, ignored, "tracker:7144")
	if addr != "tracker:7144" {
		t.Errorf("got %s, want tracker:7144", addr)
	}
}

func TestSelectSourceHost_ReturnsEmptyWhenAllIgnored(t *testing.T) {
	ignored := NewIgnoredNodeCollection()
	ignored.Add("1.2.3.4:7144")
	ignored.Add("tracker:7144")

	nodes := []SourceNode{
		{SessionID: pcp.GnuID{1}, GlobalAddr: "1.2.3.4:7144", IsReceiving: true},
	}

	addr := selectSourceHost(nodes, ignored, "tracker:7144")
	if addr != "" {
		t.Errorf("got %s, want empty (all ignored)", addr)
	}
}

func TestIgnoredNodeCollection_Expiry(t *testing.T) {
	c := &IgnoredNodeCollection{
		entries:   make(map[string]time.Time),
		threshold: 50 * time.Millisecond,
	}

	c.Add("1.2.3.4:7144")
	if !c.Contains("1.2.3.4:7144") {
		t.Error("should contain just-added entry")
	}

	time.Sleep(60 * time.Millisecond)

	if c.Contains("1.2.3.4:7144") {
		t.Error("should not contain expired entry")
	}
}

func TestIgnoredNodeCollection_NotContained(t *testing.T) {
	c := NewIgnoredNodeCollection()
	if c.Contains("1.2.3.4:7144") {
		t.Error("should not contain unknown entry")
	}
}
