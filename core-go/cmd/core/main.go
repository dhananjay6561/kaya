// Command core is Kaya's agent: it reads what the gateway stores, answers DJ and acts for him.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/dhananjay6561/kaya/core/internal/config"
	"github.com/dhananjay6561/kaya/kit/envfile"
	"github.com/dhananjay6561/kaya/kit/httpapi"
	"github.com/dhananjay6561/kaya/kit/logging"
	"github.com/dhananjay6561/kaya/kit/store"
)

var version = "dev"

const shutdownGrace = 10 * time.Second

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			exitOnError(healthcheck())
			return
		default:
			exitOnError(fmt.Errorf("unknown command %q; the only command is healthcheck", os.Args[1]))
		}
	}

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	logger := logging.New(os.Stdout, cfg.LogLevel, "service", "core", "version", version)
	if err := run(cfg, logger); err != nil {
		logger.Log(context.Background(), logging.LevelFatal, "core failed", logging.Err(err))
		os.Exit(1)
	}
}

func exitOnError(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func loadConfig() (config.Config, error) {
	envFile, err := envfile.Load()
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
	go func() {
		<-ctx.Done()
		logger.Info("core stopping")
		stop()
	}()

	db, err := store.Open(cfg.DatabaseURL, store.WithAppName("kaya-core"))
	if err != nil {
		return err
	}
	defer db.Close()
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if err := db.Check(checkCtx); err != nil {
		logger.Warn("db not reachable at startup; health reports it until it is", logging.Err(err))
	}
	cancel()

	ln, err := net.Listen("tcp", net.JoinHostPort(cfg.ListenHost, strconv.Itoa(cfg.Port)))
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	handler := httpapi.NewHandler(httpapi.Options{
		Logger:  logger,
		Service: "core",
		Version: version,
		Checks:  map[string]httpapi.Checker{"db": db},
	})
	logger.Info("core started",
		"addr", ln.Addr().String(),
		"llm", cfg.LLM.Provider,
		"channel", cfg.Channel,
		"telephony", cfg.Telephony,
		"data_dir", cfg.DataDir,
	)
	err = httpapi.Serve(ctx, httpapi.NewServer(handler, logger), ln, shutdownGrace)
	logger.Info("core stopped")
	return err
}

func healthcheck() error {
	port := os.Getenv("CORE_PORT")
	if port == "" {
		port = "3000"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	return httpapi.Check(ctx, "http://"+net.JoinHostPort("127.0.0.1", port)+"/health")
}
