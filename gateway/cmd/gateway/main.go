// Command gateway connects Kaya to WhatsApp.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/dhananjay6561/kaya/gateway/internal/config"
	"github.com/dhananjay6561/kaya/gateway/internal/contracts"
	"github.com/dhananjay6561/kaya/gateway/internal/httpapi"
	"github.com/dhananjay6561/kaya/gateway/internal/logging"
	"github.com/dhananjay6561/kaya/gateway/internal/store"
)

var version = "dev"

const shutdownGrace = 10 * time.Second

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := healthcheck(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	logger := logging.New(os.Stdout, cfg.LogLevel, "service", "gateway", "version", version)
	if err := run(cfg, logger); err != nil {
		logger.Log(context.Background(), logging.LevelFatal, "gateway failed", "error", err.Error())
		os.Exit(1)
	}
}

func loadConfig() (config.Config, error) {
	envFile, err := config.LoadEnvFile()
	if err != nil {
		return config.Config{}, err
	}
	baseDir, err := os.Getwd()
	if err != nil {
		return config.Config{}, err
	}
	if envFile != "" {
		baseDir = filepath.Dir(envFile)
	}
	return config.Load(os.Getenv, baseDir)
}

func run(cfg config.Config, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if _, err := contracts.NewValidator(); err != nil {
		return err
	}

	db, err := store.Open(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if err := db.Check(checkCtx); err != nil {
		logger.Warn("db not reachable at startup; health reports it until it is", "error", err.Error())
	}
	cancel()

	addr := net.JoinHostPort(cfg.ListenHost, strconv.Itoa(cfg.Port))
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}

	handler := httpapi.NewHandler(httpapi.Options{
		Logger:  logger,
		Service: "gateway",
		Version: version,
		Checks:  map[string]httpapi.Checker{"db": db},
	})
	logger.Info("gateway started", "addr", addr, "data_dir", cfg.DataDir)

	err = httpapi.Serve(ctx, httpapi.NewServer(handler, logger), ln, shutdownGrace)
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	logger.Info("gateway stopped")
	return nil
}

func healthcheck() error {
	port := os.Getenv("GATEWAY_PORT")
	if port == "" {
		port = "3001"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	return httpapi.Check(ctx, "http://"+net.JoinHostPort("127.0.0.1", port)+"/health")
}
