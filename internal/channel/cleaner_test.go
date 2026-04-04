package channel

import (
	"testing"
	"time"

	"github.com/titagaki/peercast-pcp/pcp"
)

func TestCleaner_RemovesIdleRelayChannel(t *testing.T) {
	mgr := NewManager(pcp.GnuID{})
	ch := New(pcp.GnuID{1}, pcp.GnuID{}, 0)
	mgr.AddRelayChannel(ch, &fakeRelay{})

	// Very short limit for testing.
	c := NewCleaner(mgr, 50*time.Millisecond)

	// First cleanup marks the channel as idle.
	c.cleanup()
	if _, ok := mgr.GetByID(ch.ID); !ok {
		t.Fatal("channel should still exist after first cleanup")
	}

	// Wait past the limit.
	time.Sleep(60 * time.Millisecond)

	// Second cleanup removes it.
	c.cleanup()
	if _, ok := mgr.GetByID(ch.ID); ok {
		t.Fatal("channel should have been removed")
	}
}

func TestCleaner_ResetsWhenViewerConnects(t *testing.T) {
	mgr := NewManager(pcp.GnuID{})
	ch := New(pcp.GnuID{2}, pcp.GnuID{}, 0)
	mgr.AddRelayChannel(ch, &fakeRelay{})

	c := NewCleaner(mgr, 80*time.Millisecond)

	// Mark as idle.
	c.cleanup()

	time.Sleep(50 * time.Millisecond)

	// Simulate a viewer connecting.
	ch.AddOutput(&fakeOutput{typ: OutputStreamHTTP})
	c.cleanup()

	// Wait past original limit — should NOT be removed (timer was reset).
	time.Sleep(50 * time.Millisecond)
	c.cleanup()
	if _, ok := mgr.GetByID(ch.ID); !ok {
		t.Fatal("channel should still exist because viewer connected")
	}
}

func TestCleaner_SkipsBroadcastChannel(t *testing.T) {
	mgr := NewManager(pcp.GnuID{})

	// Create a broadcast channel via the manager.
	mgr.IssueStreamKey("test", "key123")
	ch, err := mgr.Broadcast("key123", ChannelInfo{Name: "Test"}, TrackInfo{})
	if err != nil {
		t.Fatal(err)
	}

	c := NewCleaner(mgr, 10*time.Millisecond)
	c.cleanup()
	time.Sleep(20 * time.Millisecond)
	c.cleanup()

	if _, ok := mgr.GetByID(ch.ID); !ok {
		t.Fatal("broadcast channel should never be auto-removed")
	}
}

func TestCleaner_DisabledWithZeroLimit(t *testing.T) {
	mgr := NewManager(pcp.GnuID{})
	ch := New(pcp.GnuID{3}, pcp.GnuID{}, 0)
	mgr.AddRelayChannel(ch, &fakeRelay{})

	c := NewCleaner(mgr, 0)
	c.cleanup()
	time.Sleep(10 * time.Millisecond)
	c.cleanup()

	if _, ok := mgr.GetByID(ch.ID); !ok {
		t.Fatal("channel should not be removed when limit is 0")
	}
}

type fakeRelay struct{}

func (f *fakeRelay) Stop() {}

type fakeOutput struct {
	typ    OutputStreamType
	closed bool
}

func (f *fakeOutput) NotifyHeader()       {}
func (f *fakeOutput) NotifyInfo()          {}
func (f *fakeOutput) NotifyTrack()         {}
func (f *fakeOutput) Close()               { f.closed = true }
func (f *fakeOutput) Type() OutputStreamType { return f.typ }
func (f *fakeOutput) ID() int              { return 99 }
func (f *fakeOutput) RemoteAddr() string   { return "127.0.0.1:0" }
func (f *fakeOutput) SendRate() int64      { return 0 }
