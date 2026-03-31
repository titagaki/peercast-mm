package jsonrpc

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/titagaki/peercast-pcp/pcp"

	"github.com/titagaki/peercast-mm/internal/channel"
	"github.com/titagaki/peercast-mm/internal/config"
	"github.com/titagaki/peercast-mm/internal/version"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func newTestServer(t *testing.T) (*Server, *channel.Channel, pcp.GnuID) {
	t.Helper()
	var sid, broadcastID pcp.GnuID
	for i := range sid {
		sid[i] = byte(0xAA)
	}
	for i := range broadcastID {
		broadcastID[i] = byte(0xBB)
	}
	mgr := channel.NewManager(broadcastID)
	streamKey := mgr.IssueStreamKey()
	info := channel.ChannelInfo{
		Name:    "テストチャンネル",
		URL:     "https://example.com",
		Desc:    "説明",
		Genre:   "テスト",
		Type:    "FLV",
		Bitrate: 500,
	}
	ch, err := mgr.Broadcast(streamKey, info, channel.TrackInfo{})
	if err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	cfg := &config.Config{
		RTMPPort:     1935,
		PeercastPort: 7144,
		YPs: []config.YP{
			{Name: "TestYP", Addr: "yp.example.com:7144"},
		},
	}
	s := New(sid, mgr, cfg, nil)
	return s, ch, ch.ID
}

func chanIDHex(id pcp.GnuID) string {
	return hex.EncodeToString(id[:])
}

func post(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/1", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func rpcCall(t *testing.T, s *Server, method string, params interface{}) map[string]interface{} {
	t.Helper()
	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  method,
		"id":      1,
	}
	if params != nil {
		req["params"] = params
	} else {
		req["params"] = []interface{}{}
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	w := post(t, s, string(body))
	var resp map[string]interface{}
	if err := json.NewDecoder(bytes.NewReader(w.Body.Bytes())).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

func assertResult(t *testing.T, resp map[string]interface{}) interface{} {
	t.Helper()
	if e, ok := resp["error"]; ok {
		t.Fatalf("unexpected error: %v", e)
	}
	return resp["result"]
}

func assertError(t *testing.T, resp map[string]interface{}, wantCode int) {
	t.Helper()
	errVal, ok := resp["error"]
	if !ok {
		t.Fatalf("expected error with code %d, got result: %v", wantCode, resp["result"])
	}
	errMap := errVal.(map[string]interface{})
	code := int(errMap["code"].(float64))
	if code != wantCode {
		t.Fatalf("expected error code %d, got %d (message: %v)", wantCode, code, errMap["message"])
	}
}

// ---------------------------------------------------------------------------
// mock OutputStream
// ---------------------------------------------------------------------------

type mockStream struct {
	id         int
	streamType channel.OutputStreamType
	remoteAddr string
	sendRate   int64
	closed     bool
}

func (m *mockStream) NotifyHeader()                   {}
func (m *mockStream) NotifyInfo()                     {}
func (m *mockStream) NotifyTrack()                    {}
func (m *mockStream) Close()                          { m.closed = true }
func (m *mockStream) Type() channel.OutputStreamType  { return m.streamType }
func (m *mockStream) ID() int                         { return m.id }
func (m *mockStream) RemoteAddr() string              { return m.remoteAddr }
func (m *mockStream) SendRate() int64                 { return m.sendRate }
func (m *mockStream) SendBcst(_ *pcp.Atom)            {}

// ---------------------------------------------------------------------------
// HTTP method validation
// ---------------------------------------------------------------------------

func TestHandler_NonPost(t *testing.T) {
	s, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/1", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// JSON-RPC error cases
// ---------------------------------------------------------------------------

func TestParseError(t *testing.T) {
	s, _, _ := newTestServer(t)
	w := post(t, s, `{invalid json`)
	var resp map[string]interface{}
	json.NewDecoder(bytes.NewReader(w.Body.Bytes())).Decode(&resp)
	assertError(t, resp, errCodeParse)
}

func TestMethodNotFound(t *testing.T) {
	s, _, _ := newTestServer(t)
	resp := rpcCall(t, s, "noSuchMethod", nil)
	assertError(t, resp, errCodeMethodNotFound)
}

func TestMethodNotFound_MessageContainsName(t *testing.T) {
	s, _, _ := newTestServer(t)
	resp := rpcCall(t, s, "noSuchMethod", nil)
	msg := resp["error"].(map[string]interface{})["message"].(string)
	if !strings.Contains(msg, "noSuchMethod") {
		t.Fatalf("error message should contain method name, got: %s", msg)
	}
}

// ---------------------------------------------------------------------------
// getVersionInfo
// ---------------------------------------------------------------------------

func TestGetVersionInfo(t *testing.T) {
	s, _, _ := newTestServer(t)
	resp := rpcCall(t, s, "getVersionInfo", nil)
	result := assertResult(t, resp).(map[string]interface{})
	if result["agentName"] != version.AgentName {
		t.Fatalf("expected %s, got %v", version.AgentName, result["agentName"])
	}
}

// ---------------------------------------------------------------------------
// getSettings
// ---------------------------------------------------------------------------

func TestGetSettings(t *testing.T) {
	s, _, _ := newTestServer(t)
	resp := rpcCall(t, s, "getSettings", nil)
	result := assertResult(t, resp).(map[string]interface{})
	if int(result["serverPort"].(float64)) != 7144 {
		t.Fatalf("expected serverPort 7144, got %v", result["serverPort"])
	}
	if int(result["rtmpPort"].(float64)) != 1935 {
		t.Fatalf("expected rtmpPort 1935, got %v", result["rtmpPort"])
	}
}

// ---------------------------------------------------------------------------
// getChannels
// ---------------------------------------------------------------------------

func TestGetChannels(t *testing.T) {
	s, _, chID := newTestServer(t)
	resp := rpcCall(t, s, "getChannels", nil)
	result := assertResult(t, resp).([]interface{})
	if len(result) != 1 {
		t.Fatalf("expected 1 channel, got %d", len(result))
	}
	ch := result[0].(map[string]interface{})
	if ch["channelId"] != chanIDHex(chID) {
		t.Fatalf("channelId mismatch: got %v", ch["channelId"])
	}
	// info
	info := ch["info"].(map[string]interface{})
	if info["name"] != "テストチャンネル" {
		t.Fatalf("unexpected name: %v", info["name"])
	}
	// status — no data written to buffer, so status is Idle
	status := ch["status"].(map[string]interface{})
	if status["status"] != "Idle" {
		t.Fatalf("unexpected status: %v", status["status"])
	}
	if !status["isBroadcasting"].(bool) {
		t.Fatal("isBroadcasting should be true for broadcast channel")
	}
	// yellowPages
	yps := ch["yellowPages"].([]interface{})
	if len(yps) != 1 {
		t.Fatalf("expected 1 yp, got %d", len(yps))
	}
}

// ---------------------------------------------------------------------------
// getChannelInfo
// ---------------------------------------------------------------------------

func TestGetChannelInfo_Valid(t *testing.T) {
	s, _, chID := newTestServer(t)
	resp := rpcCall(t, s, "getChannelInfo", []interface{}{chanIDHex(chID)})
	result := assertResult(t, resp).(map[string]interface{})
	info := result["info"].(map[string]interface{})
	if info["name"] != "テストチャンネル" {
		t.Fatalf("unexpected name: %v", info["name"])
	}
	if int(info["bitrate"].(float64)) != 500 {
		t.Fatalf("unexpected bitrate: %v", info["bitrate"])
	}
	yps := result["yellowPages"].([]interface{})
	if len(yps) != 1 {
		t.Fatalf("expected 1 yp, got %d", len(yps))
	}
	yp := yps[0].(map[string]interface{})
	if int(yp["yellowPageId"].(float64)) != 0 {
		t.Fatalf("expected yellowPageId 0, got %v", yp["yellowPageId"])
	}
}

func TestGetChannelInfo_CaseInsensitiveID(t *testing.T) {
	s, _, chID := newTestServer(t)
	upperID := strings.ToUpper(chanIDHex(chID))
	resp := rpcCall(t, s, "getChannelInfo", []interface{}{upperID})
	assertResult(t, resp)
}

func TestGetChannelInfo_WrongID(t *testing.T) {
	s, _, _ := newTestServer(t)
	resp := rpcCall(t, s, "getChannelInfo", []interface{}{"00000000000000000000000000000000"})
	assertError(t, resp, errCodeInternal)
}

func TestGetChannelInfo_MissingParams(t *testing.T) {
	s, _, _ := newTestServer(t)
	resp := rpcCall(t, s, "getChannelInfo", []interface{}{})
	assertError(t, resp, errCodeInvalidParams)
}

// ---------------------------------------------------------------------------
// getChannelStatus
// ---------------------------------------------------------------------------

func TestGetChannelStatus(t *testing.T) {
	s, _, chID := newTestServer(t)
	resp := rpcCall(t, s, "getChannelStatus", []interface{}{chanIDHex(chID)})
	result := assertResult(t, resp).(map[string]interface{})
	if result["status"] != "Idle" {
		t.Fatalf("unexpected status: %v", result["status"])
	}
	// source includes the stream key: rtmp://127.0.0.1:1935/live/<streamKey>
	src, _ := result["source"].(string)
	if !strings.HasPrefix(src, "rtmp://127.0.0.1:1935/live/sk_") {
		t.Fatalf("unexpected source: %v", src)
	}
	if !result["isBroadcasting"].(bool) {
		t.Fatal("isBroadcasting should be true for broadcast channel")
	}
	if result["isRelayFull"].(bool) {
		t.Fatal("isRelayFull should be false")
	}
	if result["isDirectFull"].(bool) {
		t.Fatal("isDirectFull should be false")
	}
}

func TestGetChannelStatus_Counts(t *testing.T) {
	s, ch, chID := newTestServer(t)
	ch.AddOutput(&mockStream{id: 1, streamType: channel.OutputStreamPCP, remoteAddr: "1.2.3.4:7144"})
	ch.AddOutput(&mockStream{id: 2, streamType: channel.OutputStreamHTTP, remoteAddr: "1.2.3.5:8080"})

	resp := rpcCall(t, s, "getChannelStatus", []interface{}{chanIDHex(chID)})
	result := assertResult(t, resp).(map[string]interface{})
	if int(result["totalRelays"].(float64)) != 1 {
		t.Fatalf("expected 1 relay, got %v", result["totalRelays"])
	}
	if int(result["totalDirects"].(float64)) != 1 {
		t.Fatalf("expected 1 direct, got %v", result["totalDirects"])
	}
}

// ---------------------------------------------------------------------------
// setChannelInfo
// ---------------------------------------------------------------------------

func TestSetChannelInfo(t *testing.T) {
	s, ch, chID := newTestServer(t)
	newInfo := map[string]interface{}{
		"name":    "新チャンネル",
		"url":     "https://new.example.com",
		"desc":    "新説明",
		"comment": "コメント",
		"genre":   "新ジャンル",
		"type":    "FLV",
		"bitrate": 1000,
	}
	newTrack := map[string]interface{}{
		"title":   "曲名",
		"creator": "アーティスト",
		"url":     "",
		"album":   "アルバム",
	}
	resp := rpcCall(t, s, "setChannelInfo", []interface{}{chanIDHex(chID), newInfo, newTrack})
	result := assertResult(t, resp)
	if result != nil {
		t.Fatalf("expected null result, got %v", result)
	}

	info := ch.Info()
	if info.Name != "新チャンネル" {
		t.Fatalf("name not updated: %v", info.Name)
	}
	if info.Bitrate != 1000 {
		t.Fatalf("bitrate not updated: %v", info.Bitrate)
	}

	track := ch.Track()
	if track.Title != "曲名" {
		t.Fatalf("track title not updated: %v", track.Title)
	}
	if track.Creator != "アーティスト" {
		t.Fatalf("track creator not updated: %v", track.Creator)
	}
}

func TestSetChannelInfo_ZeroBitratePreservesExisting(t *testing.T) {
	s, ch, chID := newTestServer(t)
	newInfo := map[string]interface{}{
		"name": "名前", "url": "", "desc": "", "comment": "", "genre": "", "type": "FLV",
		"bitrate": 0,
	}
	newTrack := map[string]interface{}{"title": "", "creator": "", "url": "", "album": ""}
	rpcCall(t, s, "setChannelInfo", []interface{}{chanIDHex(chID), newInfo, newTrack})

	if ch.Info().Bitrate != 500 {
		t.Fatalf("bitrate should remain 500 when 0 is passed, got %v", ch.Info().Bitrate)
	}
}

func TestSetChannelInfo_WrongID(t *testing.T) {
	s, _, _ := newTestServer(t)
	resp := rpcCall(t, s, "setChannelInfo", []interface{}{
		"00000000000000000000000000000000",
		map[string]interface{}{"name": "x", "bitrate": 0},
		map[string]interface{}{},
	})
	assertError(t, resp, errCodeInternal)
}

func TestSetChannelInfo_MissingParams(t *testing.T) {
	s, _, chID := newTestServer(t)
	resp := rpcCall(t, s, "setChannelInfo", []interface{}{chanIDHex(chID)})
	assertError(t, resp, errCodeInvalidParams)
}

// ---------------------------------------------------------------------------
// stopChannel
// ---------------------------------------------------------------------------

func TestStopChannel(t *testing.T) {
	s, ch, chID := newTestServer(t)
	m := &mockStream{id: 1, streamType: channel.OutputStreamPCP, remoteAddr: "1.2.3.4:7144"}
	ch.AddOutput(m)

	resp := rpcCall(t, s, "stopChannel", []interface{}{chanIDHex(chID)})
	assertResult(t, resp)

	if !m.closed {
		t.Fatal("expected stream to be closed by stopChannel")
	}
}

// ---------------------------------------------------------------------------
// bumpChannel
// ---------------------------------------------------------------------------

func TestBumpChannel_NoYP(t *testing.T) {
	s, _, chID := newTestServer(t)
	// ypClient is nil — should be a no-op without panic
	resp := rpcCall(t, s, "bumpChannel", []interface{}{chanIDHex(chID)})
	assertResult(t, resp)
}

// ---------------------------------------------------------------------------
// getChannelConnections
// ---------------------------------------------------------------------------

func TestGetChannelConnections_SourceOnly(t *testing.T) {
	s, _, chID := newTestServer(t)
	resp := rpcCall(t, s, "getChannelConnections", []interface{}{chanIDHex(chID)})
	result := assertResult(t, resp).([]interface{})
	if len(result) != 1 {
		t.Fatalf("expected 1 connection (source), got %d", len(result))
	}
	src := result[0].(map[string]interface{})
	if int(src["connectionId"].(float64)) != -1 {
		t.Fatalf("source connectionId should be -1, got %v", src["connectionId"])
	}
	if src["type"] != "source" {
		t.Fatalf("expected type source, got %v", src["type"])
	}
	if src["protocolName"] != "RTMP" {
		t.Fatalf("expected RTMP, got %v", src["protocolName"])
	}
}

func TestGetChannelConnections_WithOutputs(t *testing.T) {
	s, ch, chID := newTestServer(t)
	ch.AddOutput(&mockStream{id: 3, streamType: channel.OutputStreamPCP, remoteAddr: "10.0.0.1:7144", sendRate: 65000})
	ch.AddOutput(&mockStream{id: 5, streamType: channel.OutputStreamHTTP, remoteAddr: "10.0.0.2:54321", sendRate: 65000})

	resp := rpcCall(t, s, "getChannelConnections", []interface{}{chanIDHex(chID)})
	result := assertResult(t, resp).([]interface{})
	if len(result) != 3 {
		t.Fatalf("expected 3 connections, got %d", len(result))
	}

	relay := result[1].(map[string]interface{})
	if relay["type"] != "relay" {
		t.Fatalf("expected relay, got %v", relay["type"])
	}
	if relay["protocolName"] != "PCP" {
		t.Fatalf("expected PCP, got %v", relay["protocolName"])
	}

	direct := result[2].(map[string]interface{})
	if direct["type"] != "direct" {
		t.Fatalf("expected direct, got %v", direct["type"])
	}
	if direct["protocolName"] != "HTTP" {
		t.Fatalf("expected HTTP, got %v", direct["protocolName"])
	}
}

// ---------------------------------------------------------------------------
// stopChannelConnection
// ---------------------------------------------------------------------------

func TestStopChannelConnection_PCPStream(t *testing.T) {
	s, ch, chID := newTestServer(t)
	m := &mockStream{id: 3, streamType: channel.OutputStreamPCP, remoteAddr: "10.0.0.1:7144"}
	ch.AddOutput(m)

	resp := rpcCall(t, s, "stopChannelConnection", []interface{}{chanIDHex(chID), 3})
	result := assertResult(t, resp)
	if result.(bool) != true {
		t.Fatalf("expected true, got %v", result)
	}
	if !m.closed {
		t.Fatal("stream should be closed")
	}
}

func TestStopChannelConnection_HTTPStream(t *testing.T) {
	s, ch, chID := newTestServer(t)
	m := &mockStream{id: 5, streamType: channel.OutputStreamHTTP, remoteAddr: "10.0.0.2:8080"}
	ch.AddOutput(m)

	resp := rpcCall(t, s, "stopChannelConnection", []interface{}{chanIDHex(chID), 5})
	result := assertResult(t, resp)
	// HTTP direct streams cannot be stopped — returns false
	if result.(bool) != false {
		t.Fatalf("expected false for HTTP stream, got %v", result)
	}
	if m.closed {
		t.Fatal("HTTP stream should not be closed")
	}
}

func TestStopChannelConnection_NotFound(t *testing.T) {
	s, _, chID := newTestServer(t)
	resp := rpcCall(t, s, "stopChannelConnection", []interface{}{chanIDHex(chID), 999})
	result := assertResult(t, resp)
	if result.(bool) != false {
		t.Fatalf("expected false for non-existent connection, got %v", result)
	}
}

func TestStopChannelConnection_MissingParams(t *testing.T) {
	s, _, chID := newTestServer(t)
	resp := rpcCall(t, s, "stopChannelConnection", []interface{}{chanIDHex(chID)})
	assertError(t, resp, errCodeInvalidParams)
}

// ---------------------------------------------------------------------------
// getYellowPages
// ---------------------------------------------------------------------------

func TestGetYellowPages_NoYPClient(t *testing.T) {
	s, _, _ := newTestServer(t)
	resp := rpcCall(t, s, "getYellowPages", nil)
	result := assertResult(t, resp).([]interface{})
	if len(result) != 1 {
		t.Fatalf("expected 1 yp, got %d", len(result))
	}
	yp := result[0].(map[string]interface{})
	if int(yp["yellowPageId"].(float64)) != 0 {
		t.Fatalf("expected yellowPageId 0, got %v", yp["yellowPageId"])
	}
	if yp["name"] != "TestYP" {
		t.Fatalf("unexpected name: %v", yp["name"])
	}
	// addr has no scheme → should get pcp:// prefix
	if !strings.HasPrefix(yp["uri"].(string), "pcp://") {
		t.Fatalf("expected pcp:// uri, got %v", yp["uri"])
	}
	if int(yp["channelCount"].(float64)) != 0 {
		t.Fatalf("expected channelCount 0 (no client), got %v", yp["channelCount"])
	}
}

func TestGetYellowPages_AddrWithScheme(t *testing.T) {
	var sid, broadcastID pcp.GnuID
	mgr := channel.NewManager(broadcastID)
	cfg := &config.Config{
		RTMPPort:     1935,
		PeercastPort: 7144,
		YPs:          []config.YP{{Name: "YP2", Addr: "pcp://yp.example.com/"}},
	}
	s := New(sid, mgr, cfg, nil)
	resp := rpcCall(t, s, "getYellowPages", nil)
	result := assertResult(t, resp).([]interface{})
	yp := result[0].(map[string]interface{})
	if yp["uri"] != "pcp://yp.example.com/" {
		t.Fatalf("uri should not be double-prefixed, got %v", yp["uri"])
	}
}

func TestGetYellowPages_Empty(t *testing.T) {
	var sid, broadcastID pcp.GnuID
	mgr := channel.NewManager(broadcastID)
	cfg := &config.Config{RTMPPort: 1935, PeercastPort: 7144}
	s := New(sid, mgr, cfg, nil)
	resp := rpcCall(t, s, "getYellowPages", nil)
	result := assertResult(t, resp).([]interface{})
	if len(result) != 0 {
		t.Fatalf("expected empty yp list, got %d", len(result))
	}
}

// ---------------------------------------------------------------------------
// getChannelRelayTree
// ---------------------------------------------------------------------------

func TestGetChannelRelayTree(t *testing.T) {
	s, _, chID := newTestServer(t)
	resp := rpcCall(t, s, "getChannelRelayTree", []interface{}{chanIDHex(chID)})
	result := assertResult(t, resp).([]interface{})
	if len(result) != 1 {
		t.Fatalf("expected 1 node, got %d", len(result))
	}
	node := result[0].(map[string]interface{})
	if !node["isTracker"].(bool) {
		t.Fatal("isTracker should be true")
	}
	if int(node["port"].(float64)) != 7144 {
		t.Fatalf("expected port 7144, got %v", node["port"])
	}
	if node["address"] != "" {
		t.Fatalf("address should be empty string, got %v", node["address"])
	}
	if int(node["version"].(float64)) != version.PCPVersion {
		t.Fatalf("expected version %d, got %v", version.PCPVersion, node["version"])
	}
	if node["versionString"] != version.AgentName {
		t.Fatalf("expected %s, got %v", version.AgentName, node["versionString"])
	}
	children := node["children"].([]interface{})
	if len(children) != 0 {
		t.Fatalf("children should be empty, got %v", children)
	}
	if node["isFirewalled"].(bool) {
		t.Fatal("isFirewalled should be false")
	}
	if node["isRelayFull"].(bool) {
		t.Fatal("isRelayFull should be false")
	}
	if node["isControlFull"].(bool) {
		t.Fatal("isControlFull should be false")
	}
}

// ---------------------------------------------------------------------------
// Response envelope
// ---------------------------------------------------------------------------

func TestResponseEnvelope_HasJSONRPC(t *testing.T) {
	s, _, _ := newTestServer(t)
	resp := rpcCall(t, s, "getVersionInfo", nil)
	if resp["jsonrpc"] != "2.0" {
		t.Fatalf("expected jsonrpc 2.0, got %v", resp["jsonrpc"])
	}
}

func TestResponseEnvelope_IDEchoed(t *testing.T) {
	s, _, _ := newTestServer(t)
	body := `{"jsonrpc":"2.0","method":"getVersionInfo","params":[],"id":42}`
	w := post(t, s, body)
	var resp map[string]interface{}
	json.NewDecoder(bytes.NewReader(w.Body.Bytes())).Decode(&resp)
	if int(resp["id"].(float64)) != 42 {
		t.Fatalf("expected id 42, got %v", resp["id"])
	}
}
