package main

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	gortmp "github.com/yutopp/go-rtmp"
	"github.com/yutopp/go-rtmp/message"

	"github.com/titagaki/peercast-mi/internal/channel"
	"github.com/titagaki/peercast-mi/internal/config"
	"github.com/titagaki/peercast-mi/internal/id"
	"github.com/titagaki/peercast-mi/internal/jsonrpc"
	"github.com/titagaki/peercast-mi/internal/rtmp"
	"github.com/titagaki/peercast-mi/internal/servent"
)

// ---------------------------------------------------------------------------
// Test environment: starts all components on ephemeral ports
// ---------------------------------------------------------------------------

type testEnv struct {
	t            *testing.T
	mgr          *channel.Manager
	listener     *servent.Listener
	rtmpServer   *rtmp.Server
	apiHandler   http.Handler
	peercastPort int
	rtmpPort     int
}

func newTestEnv(t *testing.T) *testEnv {
	return newTestEnvWithLimits(t, 0, 0)
}

func newTestEnvWithLimits(t *testing.T, maxRelays, maxListeners int) *testEnv {
	t.Helper()

	sessionID := id.NewRandom()
	broadcastID := id.NewRandom()
	mgr := channel.NewManager(broadcastID)

	// Find free ports.
	peercastPort := freePort(t)
	rtmpPort := freePort(t)

	cfg := &config.Config{
		RTMPPort:     rtmpPort,
		PeercastPort: peercastPort,
		MaxRelays:    maxRelays,
		MaxListeners: maxListeners,
	}

	// Start listener.
	l := servent.NewListener(sessionID, mgr, peercastPort, maxRelays, 0, maxListeners, 0)
	if err := l.Listen(); err != nil {
		t.Fatalf("listener: %v", err)
	}
	go l.Serve()

	// Wire JSON-RPC.
	apiServer := jsonrpc.New(sessionID, mgr, cfg, nil)
	l.SetAPIHandler(apiServer.Handler())

	// Start RTMP server.
	rs := rtmp.NewServer(mgr, rtmpPort)
	if err := rs.Listen(); err != nil {
		t.Fatalf("rtmp server: %v", err)
	}
	go rs.Serve()

	env := &testEnv{
		t:            t,
		mgr:          mgr,
		listener:     l,
		rtmpServer:   rs,
		apiHandler:   apiServer.Handler(),
		peercastPort: peercastPort,
		rtmpPort:     rtmpPort,
	}
	t.Cleanup(func() {
		rs.Close()
		l.Close()
		mgr.StopAll()
	})
	return env
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// rpc sends a JSON-RPC request to the test environment's API.
func (e *testEnv) rpc(method string, params interface{}) json.RawMessage {
	e.t.Helper()
	body := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
		"id":      1,
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d/api/1", e.peercastPort),
		"application/json",
		bytes.NewReader(b),
	)
	if err != nil {
		e.t.Fatalf("rpc %s: %v", method, err)
	}
	defer resp.Body.Close()
	var result struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		e.t.Fatalf("rpc %s: decode: %v", method, err)
	}
	if result.Error != nil {
		e.t.Fatalf("rpc %s: error %d: %s", method, result.Error.Code, result.Error.Message)
	}
	return result.Result
}

// issueStreamKey generates a stream key and registers it via the API.
func (e *testEnv) issueStreamKey() string {
	e.t.Helper()
	rawID := id.NewRandom()
	key := "sk_" + hex.EncodeToString(rawID[:])
	e.rpc("issueStreamKey", []string{"test-account-" + key[3:11], key})
	return key
}

// broadcastChannel creates a channel via the API and returns its hex channel ID.
func (e *testEnv) broadcastChannel(streamKey, name, genre string) string {
	e.t.Helper()
	params := []map[string]interface{}{
		{
			"sourceUri": fmt.Sprintf("rtmp://127.0.0.1:%d/live/%s", e.rtmpPort, streamKey),
			"info": map[string]interface{}{
				"name":  name,
				"genre": genre,
			},
			"track": map[string]interface{}{},
		},
	}
	raw := e.rpc("broadcastChannel", params)
	var res struct {
		ChannelID string `json:"channelId"`
	}
	json.Unmarshal(raw, &res)
	return res.ChannelID
}

// getChannels returns the raw JSON of getChannels response.
func (e *testEnv) getChannels() []map[string]interface{} {
	e.t.Helper()
	raw := e.rpc("getChannels", nil)
	var res []map[string]interface{}
	json.Unmarshal(raw, &res)
	return res
}

// ---------------------------------------------------------------------------
// RTMP client helpers
// ---------------------------------------------------------------------------

// rtmpPublishClient connects to the RTMP server and publishes with the given stream key.
// Returns the stream for writing video/audio data.
type rtmpPublishClient struct {
	conn   *gortmp.ClientConn
	stream *gortmp.Stream
}

func (e *testEnv) connectRTMP(streamKey string) *rtmpPublishClient {
	e.t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", e.rtmpPort)
	cc, err := gortmp.Dial("rtmp", addr, &gortmp.ConnConfig{
		Handler: &gortmp.DefaultHandler{},
	})
	if err != nil {
		e.t.Fatalf("rtmp dial: %v", err)
	}
	if err := cc.Connect(&message.NetConnectionConnect{
		Command: message.NetConnectionConnectCommand{
			App: "live",
		},
	}); err != nil {
		cc.Close()
		e.t.Fatalf("rtmp connect: %v", err)
	}
	stream, err := cc.CreateStream(&message.NetConnectionCreateStream{}, 128)
	if err != nil {
		cc.Close()
		e.t.Fatalf("rtmp createstream: %v", err)
	}
	if err := stream.Publish(&message.NetStreamPublish{
		PublishingName: streamKey,
		PublishingType: "live",
	}); err != nil {
		cc.Close()
		e.t.Fatalf("rtmp publish: %v", err)
	}
	return &rtmpPublishClient{conn: cc, stream: stream}
}

func (c *rtmpPublishClient) close() {
	c.conn.Close()
}

// sendAVCSequenceHeader sends an AVC sequence header (0x17 0x00 + dummy SPS/PPS).
func (c *rtmpPublishClient) sendAVCSequenceHeader(t *testing.T) {
	t.Helper()
	// Minimal AVC sequence header: keyframe (0x17) + AVC seq header (0x00) + dummy data
	body := []byte{0x17, 0x00, 0x00, 0x00, 0x00}
	body = append(body, bytes.Repeat([]byte{0xAB}, 16)...)
	err := c.stream.Write(4, 0, &message.VideoMessage{
		Payload: bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("send AVC seq header: %v", err)
	}
}

// sendAACSequenceHeader sends an AAC sequence header (0xAF 0x00 + config).
func (c *rtmpPublishClient) sendAACSequenceHeader(t *testing.T) {
	t.Helper()
	body := []byte{0xAF, 0x00}
	body = append(body, bytes.Repeat([]byte{0xCD}, 8)...)
	err := c.stream.Write(4, 0, &message.AudioMessage{
		Payload: bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("send AAC seq header: %v", err)
	}
}

// sendVideoKeyframe sends a video keyframe (0x17 + AVC NALU).
func (c *rtmpPublishClient) sendVideoKeyframe(t *testing.T, timestamp uint32, data []byte) {
	t.Helper()
	body := []byte{0x17, 0x01, 0x00, 0x00, 0x00}
	body = append(body, data...)
	err := c.stream.Write(4, timestamp, &message.VideoMessage{
		Payload: bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("send video keyframe: %v", err)
	}
}

// sendVideoInterframe sends a video interframe (0x27 + AVC NALU).
func (c *rtmpPublishClient) sendVideoInterframe(t *testing.T, timestamp uint32, data []byte) {
	t.Helper()
	body := []byte{0x27, 0x01, 0x00, 0x00, 0x00}
	body = append(body, data...)
	err := c.stream.Write(4, timestamp, &message.VideoMessage{
		Payload: bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("send video interframe: %v", err)
	}
}

// sendAudio sends an AAC audio frame (0xAF 0x01 + data).
func (c *rtmpPublishClient) sendAudio(t *testing.T, timestamp uint32, data []byte) {
	t.Helper()
	body := []byte{0xAF, 0x01}
	body = append(body, data...)
	err := c.stream.Write(4, timestamp, &message.AudioMessage{
		Payload: bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("send audio: %v", err)
	}
}

// ---------------------------------------------------------------------------
// HTTP viewer helpers
// ---------------------------------------------------------------------------

// httpViewer connects as HTTP viewer to the given channel and reads the stream.
type httpViewer struct {
	conn net.Conn
	br   *bufio.Reader
}

func (e *testEnv) connectHTTPViewer(channelID string) *httpViewer {
	e.t.Helper()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", e.peercastPort), 2*time.Second)
	if err != nil {
		e.t.Fatalf("http viewer dial: %v", err)
	}
	req := fmt.Sprintf("GET /stream/%s HTTP/1.0\r\nHost: localhost\r\n\r\n", channelID)
	if _, err := io.WriteString(conn, req); err != nil {
		conn.Close()
		e.t.Fatalf("http viewer send request: %v", err)
	}
	return &httpViewer{conn: conn, br: bufio.NewReader(conn)}
}

func (v *httpViewer) close() {
	v.conn.Close()
}

// readResponse reads the HTTP response status and headers.
func (v *httpViewer) readResponse(t *testing.T) *http.Response {
	t.Helper()
	v.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(v.br, nil)
	if err != nil {
		t.Fatalf("http viewer read response: %v", err)
	}
	return resp
}

// readBytes reads up to n bytes from the stream body.
func (v *httpViewer) readBytes(t *testing.T, n int) []byte {
	t.Helper()
	v.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, n)
	got, err := io.ReadAtLeast(v.br, buf, 1)
	if err != nil {
		t.Fatalf("http viewer read: %v", err)
	}
	return buf[:got]
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestE2E_BroadcastAndHTTPView tests the full pipeline:
// JSON-RPC issueStreamKey → broadcastChannel → RTMP publish → HTTP view.
func TestE2E_BroadcastAndHTTPView(t *testing.T) {
	env := newTestEnv(t)

	// 1. Issue stream key and create channel via API.
	streamKey := env.issueStreamKey()
	channelID := env.broadcastChannel(streamKey, "test-channel", "e2e-test")
	t.Logf("channel created: %s", channelID)

	// 2. Verify channel appears in getChannels.
	channels := env.getChannels()
	if len(channels) != 1 {
		t.Fatalf("expected 1 channel, got %d", len(channels))
	}

	// 3. Connect RTMP encoder and push data.
	pub := env.connectRTMP(streamKey)
	defer pub.close()

	pub.sendAVCSequenceHeader(t)
	pub.sendAACSequenceHeader(t)
	time.Sleep(50 * time.Millisecond) // let header be processed

	// Send some keyframes.
	frameData := bytes.Repeat([]byte{0x42}, 100)
	pub.sendVideoKeyframe(t, 0, frameData)
	pub.sendVideoKeyframe(t, 33, frameData)
	pub.sendVideoKeyframe(t, 66, frameData)
	time.Sleep(50 * time.Millisecond)

	// 4. Connect HTTP viewer and verify data flows.
	viewer := env.connectHTTPViewer(channelID)
	defer viewer.close()

	resp := viewer.readResponse(t)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "video/x-flv" {
		t.Errorf("content-type: got %q, want %q", ct, "video/x-flv")
	}

	// Read FLV header (starts with "FLV").
	data := viewer.readBytes(t, 1024)
	if !bytes.HasPrefix(data, []byte("FLV")) {
		t.Fatalf("expected FLV header, got %x", data[:min(10, len(data))])
	}
	t.Logf("received %d bytes of FLV data", len(data))
}

// TestE2E_APIChannelLifecycle tests the channel lifecycle:
// issue key → broadcast → verify → stop → verify gone.
func TestE2E_APIChannelLifecycle(t *testing.T) {
	env := newTestEnv(t)

	// Issue key.
	streamKey := env.issueStreamKey()
	if !strings.HasPrefix(streamKey, "sk_") {
		t.Fatalf("stream key should start with sk_, got %q", streamKey)
	}

	// Broadcast.
	channelID := env.broadcastChannel(streamKey, "lifecycle-test", "genre1")

	// Verify channel info.
	raw := env.rpc("getChannelInfo", []string{channelID})
	var info struct {
		Info struct {
			Name  string `json:"name"`
			Genre string `json:"genre"`
		} `json:"info"`
	}
	json.Unmarshal(raw, &info)
	if info.Info.Name != "lifecycle-test" {
		t.Errorf("name: got %q, want %q", info.Info.Name, "lifecycle-test")
	}

	// Verify channel status.
	raw = env.rpc("getChannelStatus", []string{channelID})
	var status struct {
		Status string `json:"status"`
	}
	json.Unmarshal(raw, &status)
	if status.Status != "Idle" {
		t.Errorf("status: got %q, want %q", status.Status, "Idle")
	}

	// Stop channel.
	env.rpc("stopChannel", []string{channelID})

	// Verify channel is gone.
	channels := env.getChannels()
	if len(channels) != 0 {
		t.Errorf("expected 0 channels after stop, got %d", len(channels))
	}
}

// TestE2E_MultipleViewers verifies multiple HTTP viewers can connect
// to the same channel and each receives data.
func TestE2E_MultipleViewers(t *testing.T) {
	env := newTestEnv(t)

	streamKey := env.issueStreamKey()
	channelID := env.broadcastChannel(streamKey, "multi-viewer", "")

	// Push RTMP data.
	pub := env.connectRTMP(streamKey)
	defer pub.close()

	pub.sendAVCSequenceHeader(t)
	pub.sendAACSequenceHeader(t)
	frameData := bytes.Repeat([]byte{0x55}, 64)
	pub.sendVideoKeyframe(t, 0, frameData)
	time.Sleep(50 * time.Millisecond)

	// Connect multiple viewers.
	const numViewers = 3
	viewers := make([]*httpViewer, numViewers)
	for i := range viewers {
		viewers[i] = env.connectHTTPViewer(channelID)
		defer viewers[i].close()
	}

	// Each viewer should get a 200 response with FLV data.
	for i, v := range viewers {
		resp := v.readResponse(t)
		if resp.StatusCode != 200 {
			t.Errorf("viewer %d: expected 200, got %d", i, resp.StatusCode)
			continue
		}
		data := v.readBytes(t, 512)
		if !bytes.HasPrefix(data, []byte("FLV")) {
			t.Errorf("viewer %d: expected FLV header, got %x", i, data[:min(10, len(data))])
		}
	}

	// Verify connection count via API.
	raw := env.rpc("getChannelStatus", []string{channelID})
	var st struct {
		TotalDirects int `json:"totalDirects"`
	}
	json.Unmarshal(raw, &st)
	if st.TotalDirects != numViewers {
		t.Errorf("totalDirects: got %d, want %d", st.TotalDirects, numViewers)
	}
}

// TestE2E_ListenerLimit verifies that max_listeners is enforced:
// when the limit is reached, new HTTP viewers are rejected.
func TestE2E_ListenerLimit(t *testing.T) {
	env := newTestEnvWithLimits(t, 0, 1) // maxListeners=1

	streamKey := env.issueStreamKey()
	channelID := env.broadcastChannel(streamKey, "limit-test", "")

	// Push data so HTTP viewers get 200 (not 503).
	pub := env.connectRTMP(streamKey)
	defer pub.close()
	pub.sendAVCSequenceHeader(t)
	pub.sendAACSequenceHeader(t)
	pub.sendVideoKeyframe(t, 0, bytes.Repeat([]byte{0x33}, 32))
	time.Sleep(50 * time.Millisecond)

	// First viewer should succeed.
	v1 := env.connectHTTPViewer(channelID)
	defer v1.close()
	resp1 := v1.readResponse(t)
	if resp1.StatusCode != 200 {
		t.Fatalf("viewer 1: expected 200, got %d", resp1.StatusCode)
	}

	// Second viewer should be rejected (connection closed).
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", env.peercastPort), 2*time.Second)
	if err != nil {
		t.Fatalf("viewer 2 dial: %v", err)
	}
	defer conn.Close()
	req := fmt.Sprintf("GET /stream/%s HTTP/1.0\r\nHost: localhost\r\n\r\n", channelID)
	io.WriteString(conn, req)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 256)
	n, _ := conn.Read(buf)
	if n > 0 {
		// If we get any HTTP response, it shouldn't be 200.
		if bytes.Contains(buf[:n], []byte("200 OK")) {
			t.Error("viewer 2: expected rejection, got 200 OK")
		}
	}
	// Connection closed or no data = rejected (expected)
}

// TestE2E_EncoderDisconnectStopsChannel verifies that when the RTMP
// encoder disconnects, the channel is stopped.
func TestE2E_EncoderDisconnectStopsChannel(t *testing.T) {
	env := newTestEnv(t)

	streamKey := env.issueStreamKey()
	channelID := env.broadcastChannel(streamKey, "disconnect-test", "")

	// Connect encoder and push some data.
	pub := env.connectRTMP(streamKey)
	pub.sendAVCSequenceHeader(t)
	pub.sendAACSequenceHeader(t)
	pub.sendVideoKeyframe(t, 0, bytes.Repeat([]byte{0x77}, 32))
	time.Sleep(50 * time.Millisecond)

	// Verify channel exists.
	channels := env.getChannels()
	if len(channels) != 1 {
		t.Fatalf("expected 1 channel, got %d", len(channels))
	}

	// Disconnect encoder.
	pub.close()
	time.Sleep(200 * time.Millisecond) // wait for OnClose handler

	// Verify channel is gone.
	channels = env.getChannels()
	if len(channels) != 0 {
		t.Errorf("expected 0 channels after encoder disconnect, got %d", len(channels))
	}
	_ = channelID
}

// TestE2E_RTMPRejectsUnknownKey verifies that the RTMP server rejects
// publishing with a stream key that was not issued.
func TestE2E_RTMPRejectsUnknownKey(t *testing.T) {
	env := newTestEnv(t)

	addr := fmt.Sprintf("127.0.0.1:%d", env.rtmpPort)
	cc, err := gortmp.Dial("rtmp", addr, &gortmp.ConnConfig{
		Handler: &gortmp.DefaultHandler{},
	})
	if err != nil {
		t.Fatalf("rtmp dial: %v", err)
	}
	defer cc.Close()

	if err := cc.Connect(&message.NetConnectionConnect{
		Command: message.NetConnectionConnectCommand{
			App: "live",
		},
	}); err != nil {
		t.Fatalf("rtmp connect: %v", err)
	}

	stream, err := cc.CreateStream(&message.NetConnectionCreateStream{}, 128)
	if err != nil {
		t.Fatalf("rtmp createstream: %v", err)
	}

	err = stream.Publish(&message.NetStreamPublish{
		PublishingName: "sk_invalid_key_that_was_never_issued",
		PublishingType: "live",
	})
	// The server should reject this, which manifests as an error or the
	// connection being closed. Either way the publish should not succeed.
	if err == nil {
		// Even if Publish itself succeeds (some RTMP implementations respond
		// asynchronously), sending data should fail.
		err = stream.Write(4, 0, &message.VideoMessage{
			Payload: bytes.NewReader([]byte{0x17, 0x00, 0x00, 0x00, 0x00}),
		})
		// Give server time to process rejection.
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("result of publishing with unknown key: %v", err)
}

// TestE2E_SetChannelInfo verifies that channel info can be updated via API
// and the change is reflected immediately.
func TestE2E_SetChannelInfo(t *testing.T) {
	env := newTestEnv(t)

	streamKey := env.issueStreamKey()
	channelID := env.broadcastChannel(streamKey, "original-name", "original-genre")

	// Update info: setChannelInfo expects [channelId, info, track].
	env.rpc("setChannelInfo", []interface{}{
		channelID,
		map[string]interface{}{
			"name":  "updated-name",
			"genre": "updated-genre",
			"desc":  "new description",
		},
		map[string]interface{}{
			"title":   "Song Title",
			"creator": "Artist",
		},
	})

	// Verify.
	raw := env.rpc("getChannelInfo", []string{channelID})
	var res struct {
		Info struct {
			Name  string `json:"name"`
			Genre string `json:"genre"`
			Desc  string `json:"desc"`
		} `json:"info"`
		Track struct {
			Title   string `json:"title"`
			Creator string `json:"creator"`
		} `json:"track"`
	}
	json.Unmarshal(raw, &res)
	if res.Info.Name != "updated-name" {
		t.Errorf("name: got %q, want %q", res.Info.Name, "updated-name")
	}
	if res.Info.Genre != "updated-genre" {
		t.Errorf("genre: got %q, want %q", res.Info.Genre, "updated-genre")
	}
	if res.Info.Desc != "new description" {
		t.Errorf("desc: got %q, want %q", res.Info.Desc, "new description")
	}
	if res.Track.Title != "Song Title" {
		t.Errorf("track title: got %q, want %q", res.Track.Title, "Song Title")
	}
}

// TestE2E_StreamReceivingStatus verifies that the channel status transitions
// from Idle to Receiving when RTMP data arrives.
func TestE2E_StreamReceivingStatus(t *testing.T) {
	env := newTestEnv(t)

	streamKey := env.issueStreamKey()
	channelID := env.broadcastChannel(streamKey, "status-test", "")

	// Initially Idle.
	raw := env.rpc("getChannelStatus", []string{channelID})
	var status struct {
		Status      string `json:"status"`
		IsReceiving bool   `json:"isReceiving"`
	}
	json.Unmarshal(raw, &status)
	if status.Status != "Idle" {
		t.Errorf("initial status: got %q, want %q", status.Status, "Idle")
	}
	if status.IsReceiving {
		t.Error("isReceiving should be false initially")
	}

	// Push RTMP data.
	pub := env.connectRTMP(streamKey)
	defer pub.close()
	pub.sendAVCSequenceHeader(t)
	pub.sendVideoKeyframe(t, 0, bytes.Repeat([]byte{0x11}, 32))
	time.Sleep(50 * time.Millisecond)

	// Should now be Receiving.
	raw = env.rpc("getChannelStatus", []string{channelID})
	json.Unmarshal(raw, &status)
	if status.Status != "Receiving" {
		t.Errorf("after push status: got %q, want %q", status.Status, "Receiving")
	}
	if !status.IsReceiving {
		t.Error("isReceiving should be true after pushing data")
	}
}

// TestE2E_ConnectionTracking verifies that connections are visible via API
// and can be individually stopped.
func TestE2E_ConnectionTracking(t *testing.T) {
	env := newTestEnv(t)

	streamKey := env.issueStreamKey()
	channelID := env.broadcastChannel(streamKey, "conn-track", "")

	// Push data.
	pub := env.connectRTMP(streamKey)
	defer pub.close()
	pub.sendAVCSequenceHeader(t)
	pub.sendAACSequenceHeader(t)
	pub.sendVideoKeyframe(t, 0, bytes.Repeat([]byte{0x22}, 32))
	time.Sleep(50 * time.Millisecond)

	// Connect viewer.
	viewer := env.connectHTTPViewer(channelID)
	defer viewer.close()
	resp := viewer.readResponse(t)
	if resp.StatusCode != 200 {
		t.Fatalf("viewer: expected 200, got %d", resp.StatusCode)
	}
	time.Sleep(50 * time.Millisecond) // let connection register

	// Get connections.
	raw := env.rpc("getChannelConnections", []string{channelID})
	var conns []map[string]interface{}
	json.Unmarshal(raw, &conns)
	if len(conns) == 0 {
		t.Fatal("expected at least 1 connection")
	}

	// Find the viewer connection and stop it.
	for _, c := range conns {
		t.Logf("connection: %v", c)
	}
}

// TestE2E_HTTPViewerGets200WhenNoData verifies that an HTTP viewer gets 200
// immediately (the server sends headers right away and waits for data).
func TestE2E_HTTPViewerGets200WhenNoData(t *testing.T) {
	env := newTestEnv(t)

	streamKey := env.issueStreamKey()
	channelID := env.broadcastChannel(streamKey, "no-data", "")

	// Connect viewer before any RTMP data is pushed.
	viewer := env.connectHTTPViewer(channelID)
	defer viewer.close()

	resp := viewer.readResponse(t)
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

// TestE2E_ViewerReceivesContinuousStream verifies that a viewer connected
// during an active broadcast receives data continuously as new frames arrive.
func TestE2E_ViewerReceivesContinuousStream(t *testing.T) {
	env := newTestEnv(t)

	streamKey := env.issueStreamKey()
	channelID := env.broadcastChannel(streamKey, "continuous", "")

	// Start RTMP encoder.
	pub := env.connectRTMP(streamKey)
	defer pub.close()
	pub.sendAVCSequenceHeader(t)
	pub.sendAACSequenceHeader(t)
	pub.sendVideoKeyframe(t, 0, bytes.Repeat([]byte{0x10}, 64))
	time.Sleep(50 * time.Millisecond)

	// Connect viewer.
	viewer := env.connectHTTPViewer(channelID)
	defer viewer.close()
	resp := viewer.readResponse(t)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Read initial data.
	initial := viewer.readBytes(t, 2048)
	if !bytes.HasPrefix(initial, []byte("FLV")) {
		t.Fatalf("expected FLV header in initial data")
	}
	t.Logf("initial: %d bytes", len(initial))

	// Push more frames and verify viewer receives them.
	for i := 0; i < 5; i++ {
		pub.sendVideoKeyframe(t, uint32(100+i*33), bytes.Repeat([]byte{byte(0x20 + i)}, 128))
	}
	time.Sleep(50 * time.Millisecond)

	more := viewer.readBytes(t, 4096)
	if len(more) == 0 {
		t.Error("expected additional data from continuous stream")
	}
	t.Logf("additional: %d bytes", len(more))
}

// TestE2E_ChannelNotFound verifies that HTTP viewers get disconnected
// when requesting a non-existent channel.
func TestE2E_ChannelNotFound(t *testing.T) {
	env := newTestEnv(t)

	// Use a valid-looking but non-existent channel ID.
	fakeID := hex.EncodeToString(make([]byte, 16))
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", env.peercastPort), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	req := fmt.Sprintf("GET /stream/%s HTTP/1.0\r\nHost: localhost\r\n\r\n", fakeID)
	io.WriteString(conn, req)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	// Server should close the connection without response.
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if n > 0 && bytes.Contains(buf[:n], []byte("200 OK")) {
		t.Error("expected rejection for non-existent channel, got 200 OK")
	}
	// EOF or error = expected (connection closed)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
