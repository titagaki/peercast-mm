package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/titagaki/peercast-mm/internal/channel"
	"github.com/titagaki/peercast-mm/internal/config"
	"github.com/titagaki/peercast-mm/internal/id"
	"github.com/titagaki/peercast-mm/internal/jsonrpc"
	"github.com/titagaki/peercast-mm/internal/rtmp"
	"github.com/titagaki/peercast-mm/internal/servent"
	"github.com/titagaki/peercast-mm/internal/yp"
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

	slog.Info("startup", "session_id", sessionID, "broadcast_id", broadcastID)

	mgr := channel.NewManager(broadcastID)

	// Start OutputListener.
	listener := servent.NewListener(sessionID, mgr, cfg.PeercastPort, cfg.MaxRelays, cfg.MaxListeners)
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
		ypClient := yp.New(hostPort, sessionID, broadcastID, mgr, cfg.PeercastPort)
		ypBumper = ypClient
		go func() {
			slog.Info("yp: connecting", "addr", ypEntry.Addr, "name", ypEntry.Name)
			ypClient.Run()
		}()
		defer ypClient.Stop()
	}

	// Wire JSON-RPC API handler into the listener.
	apiServer := jsonrpc.New(sessionID, mgr, cfg, ypBumper)
	listener.SetAPIHandler(apiServer.Handler())
	slog.Info("api: JSON-RPC ready", "port", cfg.PeercastPort)

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
