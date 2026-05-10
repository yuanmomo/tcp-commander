// tcpcommanderd is the remote command execution daemon.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yuanmomo/tcp-commander/internal/config"
	"github.com/yuanmomo/tcp-commander/internal/logging"
	"github.com/yuanmomo/tcp-commander/internal/server"
)

func main() {
	cfgPath := flag.String("config", os.Getenv("CMD_CONFIG"), "path to YAML config file (env: CMD_CONFIG)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("tcp-commander", server.Version)
		return
	}
	if *cfgPath == "" {
		fmt.Fprintln(os.Stderr, "error: --config is required (or set CMD_CONFIG)")
		os.Exit(2)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}

	log, closer, err := logging.Setup(cfg.LogConfig())
	if err != nil {
		fmt.Fprintln(os.Stderr, "log setup:", err)
		os.Exit(1)
	}
	defer closer.Close()

	srv, err := server.New(cfg, log)
	if err != nil {
		log.Error("server init", "err", err.Error())
		os.Exit(1)
	}
	if err := srv.Start(); err != nil {
		log.Error("server start", "err", err.Error())
		os.Exit(1)
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigs
	log.Info("shutdown signal received", "signal", sig.String())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Warn("shutdown timed out", "err", err.Error())
	}
	log.Info("daemon stopped")
}
