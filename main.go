package main

import (
	"context"
	"encoding/hex"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/titagaki/peercast-pcp/pcp"

	"github.com/titagaki/peercast-mi/internal/channel"
	"github.com/titagaki/peercast-mi/internal/config"
	"github.com/titagaki/peercast-mi/internal/id"
	"github.com/titagaki/peercast-mi/internal/jsonrpc"
	"github.com/titagaki/peercast-mi/internal/relay"
	"github.com/titagaki/peercast-mi/internal/rtmp"
	"github.com/titagaki/peercast-mi/internal/servent"
	"github.com/titagaki/peercast-mi/internal/yp"
)

func main() {
	configPath := flag.String("config", "config.toml", "Path to config file")
	ypName := flag.String("yp", "", "YP name to use (default: first entry in config)")
	flag.Parse()

	// Minimal logger before config is loaded (errors only).
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.SlogLevel()})))

	// Create a context that is cancelled on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	sessionID := id.NewRandom()
	broadcastID := id.NewRandom()
	var globalIP atomic.Uint32

	slog.Info("startup", "session_id", sessionID, "broadcast_id", broadcastID)

	mgr := channel.NewManager(broadcastID)
	mgr.ContentBufferSeconds = cfg.ContentBufferSeconds
	cachePath := filepath.Join(filepath.Dir(*configPath), "stream_keys.json")
	mgr.SetCachePath(cachePath)
	if err := mgr.LoadCache(); err != nil {
		slog.Warn("stream key cache: load failed", "err", err)
	}

	// Start OutputListener.
	listener := servent.NewListener(sessionID, mgr, cfg.PeercastPort, cfg.MaxRelays, cfg.MaxRelaysTotal, cfg.MaxListeners, cfg.MaxUpstreamKbps)
	if err := listener.Listen(); err != nil {
		slog.Error("output: listen failed", "err", err)
		os.Exit(1)
	}
	slog.Info("output: listening", "port", cfg.PeercastPort)
	go func() {
		if err := listener.Serve(); err != nil {
			slog.Error("output: listener stopped", "err", err)
		}
	}()

	// JSON-RPC API will be wired after YP setup (ypBumper may be nil).
	var ypBumper jsonrpc.YPBumper

	// Start YPClient if configured.
	ypEntry, err := cfg.FindYP(*ypName)
	if err != nil {
		slog.Info("yp: skipping", "reason", err)
	} else {
		hostPort, err := ypEntry.HostPort()
		if err != nil {
			slog.Error("yp: invalid addr", "addr", ypEntry.Addr, "err", err)
			os.Exit(1)
		}
		ypClient := yp.New(hostPort, sessionID, broadcastID, mgr, cfg.PeercastPort, cfg.MaxRelays, cfg.MaxListeners)
		ypClient.OnGlobalIP = func(ip uint32) {
			slog.Debug("global IP acquired", "ip", pcp.IPv4FromUint32(ip))
			globalIP.Store(ip)
			listener.SetGlobalIP(ip)
			mgr.SetGlobalIPForRelays(ip)
		}
		ypBumper = ypClient
		go func() {
			slog.Info("yp: connecting", "addr", ypEntry.Addr, "name", ypEntry.Name)
			ypClient.Run()
		}()
		defer ypClient.Stop()
	}

	// Wire on-demand relay: auto-start relay when /pls/ is requested with a tip.
	listener.OnDemandRelay = func(channelID pcp.GnuID, upstreamAddr string) error {
		if _, ok := mgr.GetByID(channelID); ok {
			return nil // already relaying
		}
		ch := channel.New(channelID, pcp.GnuID{}, 0)
		ch.SetSource(upstreamAddr)
		ch.SetUpstreamAddr(upstreamAddr)
		client := relay.New(upstreamAddr, channelID, sessionID, uint16(cfg.PeercastPort), ch)
		client.SetGlobalIP(globalIP.Load())
		mgr.AddRelayChannel(ch, client)
		go client.Run()
		slog.Info("pls: auto-relay started", "addr", upstreamAddr, "channel", hex.EncodeToString(channelID[:]))
		return nil
	}

	// Wire JSON-RPC API handler into the listener.
	apiServer := jsonrpc.New(sessionID, mgr, cfg, ypBumper)
	listener.SetAPIHandler(apiServer.Handler())
	slog.Info("api: JSON-RPC ready", "port", cfg.PeercastPort)

	// Start channel cleaner for idle relay channels.
	if cfg.ChannelCleanupMinutes > 0 {
		cleaner := channel.NewCleaner(mgr, time.Duration(cfg.ChannelCleanupMinutes)*time.Minute)
		go cleaner.Run()
		defer cleaner.Stop()
	}

	// Start RTMP server.
	rtmpServer := rtmp.NewServer(mgr, cfg.RTMPPort)
	if err := rtmpServer.Listen(); err != nil {
		slog.Error("rtmp: listen failed", "err", err)
		os.Exit(1)
	}
	slog.Info("rtmp: listening", "port", cfg.RTMPPort)
	go func() {
		if err := rtmpServer.Serve(); err != nil {
			slog.Error("rtmp: server stopped", "err", err)
		}
	}()

	// Wait for shutdown signal.
	<-ctx.Done()

	slog.Info("shutting down")
	rtmpServer.Close()
	listener.Close()
	mgr.StopAll()
}
