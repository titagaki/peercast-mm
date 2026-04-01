package jsonrpc

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/titagaki/peercast-mi/internal/channel"
)

// ---------------------------------------------------------------------------
// Request / response types
// ---------------------------------------------------------------------------

type broadcastChannelParam struct {
	SourceURI string       `json:"sourceUri"`
	Info      chanInfoArg  `json:"info"`
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
	source := ch.Source()
	if source == "" {
		source = fmt.Sprintf("rtmp://127.0.0.1:%d/live", s.cfg.RTMPPort)
		if key, ok := s.mgr.StreamKeyByID(ch.ID); ok {
			source = fmt.Sprintf("rtmp://127.0.0.1:%d/live/%s", s.cfg.RTMPPort, key)
		}
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
		IsBroadcasting: ch.IsBroadcasting(),
		IsRelayFull:    ch.IsRelayFull(s.cfg.MaxRelays),
		IsDirectFull:   ch.IsDirectFull(s.cfg.MaxListeners),
		IsReceiving:    receiving,
	}
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (s *Server) issueStreamKey(params json.RawMessage) (interface{}, *rpcError) {
	var args []string
	if err := json.Unmarshal(params, &args); err != nil || len(args) < 2 {
		return nil, &rpcError{Code: errCodeInvalidParams, Message: "expected [accountName, streamKey]"}
	}
	accountName, streamKey := args[0], args[1]
	if accountName == "" || streamKey == "" {
		return nil, &rpcError{Code: errCodeInvalidParams, Message: "accountName and streamKey must not be empty"}
	}
	if err := s.mgr.IssueStreamKey(accountName, streamKey); err != nil {
		return nil, &rpcError{Code: errCodeInternal, Message: err.Error()}
	}
	return nil, nil
}

func (s *Server) revokeStreamKey(params json.RawMessage) (interface{}, *rpcError) {
	var args []string
	if err := json.Unmarshal(params, &args); err != nil || len(args) < 1 {
		return nil, &rpcError{Code: errCodeInvalidParams, Message: "expected [accountName]"}
	}
	accountName := args[0]
	if !s.mgr.RevokeStreamKey(accountName) {
		return nil, &rpcError{Code: errCodeInternal, Message: "account not found"}
	}
	return nil, nil
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
