package pcputil

import (
	"github.com/titagaki/peercast-pcp/pcp"

	"github.com/titagaki/peercast-mi/internal/version"
)

// HostAtomParams holds the parameters needed to build a PCPHost atom.
type HostAtomParams struct {
	SessionID    pcp.GnuID
	GlobalIP     uint32
	ListenPort   uint16
	ChannelID    pcp.GnuID
	NumListeners int
	NumRelays    int
	Uptime       uint32
	OldPos       uint32
	NewPos       uint32
	IsTracker    bool
	HasGlobalIP  bool

	// TrackerAtom adds an explicit pcp.PCPHostTracker atom (used in YP bcst).
	TrackerAtom bool

	// Optional upstream host info (for relay/YP bcst).
	UphostIP   uint32
	UphostPort uint16
	UphostHops uint32
}

// BuildHostAtom constructs a PCPHost atom from the given parameters.
func BuildHostAtom(p HostAtomParams) *pcp.Atom {
	flags := byte(pcp.PCPHostFlags1Relay | pcp.PCPHostFlags1Recv | pcp.PCPHostFlags1CIN)
	if p.HasGlobalIP {
		flags |= pcp.PCPHostFlags1Direct
	}
	if p.IsTracker {
		flags |= pcp.PCPHostFlags1Tracker
	}

	var tracker uint32
	if p.TrackerAtom {
		tracker = 1
	}

	h := pcp.HostPacket{
		ID:              p.SessionID,
		IP:              p.GlobalIP,
		Port:            p.ListenPort,
		NumListeners:    uint32(p.NumListeners),
		NumRelays:       uint32(p.NumRelays),
		Uptime:          p.Uptime,
		OldPos:          p.OldPos,
		NewPos:          p.NewPos,
		ChanID:          p.ChannelID,
		Flags1:          flags,
		Version:         version.PCPVersion,
		VersionVP:       version.PCPVersionVP,
		VersionExPrefix: [2]byte([]byte(version.ExPrefix)),
		VersionExNumber: version.ExNumber(),
		Tracker:         tracker,
		UphostIP:        p.UphostIP,
		UphostPort:      uint32(p.UphostPort),
		UphostHops:      p.UphostHops,
	}
	return h.BuildAtom()
}
