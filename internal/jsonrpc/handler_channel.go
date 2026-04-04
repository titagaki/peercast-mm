package jsonrpc

import (
	"encoding/json"
	"fmt"

	"github.com/titagaki/peercast-mi/internal/channel"
)

// ---------------------------------------------------------------------------
// Request / response types
// ---------------------------------------------------------------------------

type broadcastChannelParam struct {
	StreamKey string       `json:"streamKey"`
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
	Name        string `json:"name"`
	URL         string `json:"url"`
	Genre       string `json:"genre"`
	Desc        string `json:"desc"`
	Comment     string `json:"comment"`
	Bitrate     uint32 `json:"bitrate"`
	ContentType string `json:"contentType"`
	MIMEType    string `json:"mimeType"`
}

type trackInfoResult struct {
	Name    string `json:"name"`
	Genre   string `json:"genre"`
	Album   string `json:"album"`
	Creator string `json:"creator"`
	URL     string `json:"url"`
}

type chanStatusResult struct {
	Status         string `json:"status"`
	Source         string `json:"source"`
	Uptime         uint32 `json:"uptime"`
	LocalRelays    int    `json:"localRelays"`
	LocalDirects   int    `json:"localDirects"`
	TotalRelays    int    `json:"totalRelays"`
	TotalDirects   int    `json:"totalDirects"`
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
		Name:        info.Name,
		URL:         info.URL,
		Genre:       info.Genre,
		Desc:        info.Desc,
		Comment:     info.Comment,
		Bitrate:     info.Bitrate,
		ContentType: info.Type,
		MIMEType:    info.MIMEType,
	}
}

func (s *Server) buildTrackInfo(ch *channel.Channel) trackInfoResult {
	track := ch.Track()
	return trackInfoResult{
		Name:    track.Title,
		Genre:   "",
		Album:   track.Album,
		Creator: track.Creator,
		URL:     track.URL,
	}
}

func (s *Server) buildStatus(ch *channel.Channel) chanStatusResult {
	source := ch.Source()
	if source == "" {
		source = fmt.Sprintf("rtmp://127.0.0.1:%d/live", s.cfg.RTMPPort)
		if key, ok := s.mgr.StreamKeyByID(ch.ID); ok {
			source = fmt.Sprintf("rtmp://127.0.0.1:%d/live/%s", s.cfg.RTMPPort, key)
		}
	}
	receiving := ch.HasData()
	status := "Idle"
	if receiving {
		status = "Receiving"
	}
	return chanStatusResult{
		Status:         status,
		Source:         source,
		Uptime:         ch.UptimeSeconds(),
		LocalRelays:    ch.NumRelays(),
		LocalDirects:   ch.NumListeners(),
		TotalRelays:    ch.NumRelays(),
		TotalDirects:   ch.NumListeners(),
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
		return nil, &rpcError{Code: errCodeInvalidParams, Message: "expected [{streamKey, info, track}]"}
	}
	p := args[0]

	if p.Info.Name == "" {
		return nil, &rpcError{Code: errCodeInvalidParams, Message: "channel name must not be empty"}
	}

	if p.StreamKey == "" {
		return nil, &rpcError{Code: errCodeInvalidParams, Message: "streamKey must not be empty"}
	}
	streamKey := p.StreamKey

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

	if s.ypClient != nil {
		s.ypClient.Bump()
	}

	return map[string]string{"channelId": gnuIDString(ch.ID)}, nil
}


func (s *Server) getChannels() (interface{}, *rpcError) {
	type chanEntry struct {
		ChannelID string           `json:"channelId"`
		Status    chanStatusResult `json:"status"`
		Info      chanInfoResult   `json:"info"`
		Track     trackInfoResult  `json:"track"`
	}
	channels := s.mgr.List()
	entries := make([]chanEntry, len(channels))
	for i, ch := range channels {
		entries[i] = chanEntry{
			ChannelID: gnuIDString(ch.ID),
			Status:    s.buildStatus(ch),
			Info:      s.buildChanInfo(ch),
			Track:     s.buildTrackInfo(ch),
		}
	}
	return entries, nil
}

func (s *Server) getChannelInfo(ch *channel.Channel) (interface{}, *rpcError) {
	type result struct {
		Info  chanInfoResult  `json:"info"`
		Track trackInfoResult `json:"track"`
	}
	return result{
		Info:  s.buildChanInfo(ch),
		Track: s.buildTrackInfo(ch),
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
