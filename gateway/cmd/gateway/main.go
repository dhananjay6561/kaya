// Command gateway connects Kaya to WhatsApp.
package main

import (
	"context"
	"errors"
	"flag"
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
	"github.com/dhananjay6561/kaya/gateway/internal/events"
	"github.com/dhananjay6561/kaya/gateway/internal/httpapi"
	"github.com/dhananjay6561/kaya/gateway/internal/ingest"
	"github.com/dhananjay6561/kaya/gateway/internal/logging"
	"github.com/dhananjay6561/kaya/gateway/internal/media"
	"github.com/dhananjay6561/kaya/gateway/internal/store"
	"github.com/dhananjay6561/kaya/gateway/internal/whatsapp"
)

var version = "dev"

const shutdownGrace = 10 * time.Second

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			exitOnError(healthcheck())
			return
		case "pair":
			exitOnError(pair(os.Args[2:]))
			return
		case "media":
			exitOnError(mediaCommand(os.Args[2:]))
			return
		default:
			exitOnError(fmt.Errorf("unknown command %q; the commands are healthcheck, pair and media", os.Args[1]))
		}
	}

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	logger := logging.New(os.Stdout, cfg.LogLevel, "service", "gateway", "version", version)
	if err := run(cfg, logger); err != nil {
		logger.Log(context.Background(), logging.LevelFatal, "gateway failed", logging.Err(err))
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
	context.AfterFunc(ctx, stop)

	validator, err := contracts.NewValidator()
	if err != nil {
		return err
	}

	db, err := store.Open(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	sqlDB := db.SQL()
	defer func() { _ = sqlDB.Close() }()

	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if err := db.Check(checkCtx); err != nil {
		logger.Warn("db not reachable at startup; health reports it until it is", logging.Err(err))
	}
	cancel()

	addr := net.JoinHostPort(cfg.ListenHost, strconv.Itoa(cfg.Port))
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}

	outbox := events.NewOutbox(validator)
	ingester := ingest.NewIngester(db, ingest.NewStore(outbox), logger)
	conn := whatsapp.NewConn(whatsapp.NewSessions(sqlDB, whatsapp.NewLogger(logger)), ingester, logger)
	conn.SetPublisher(events.NewPublisher(db, outbox))
	ingester.NewGroup = conn.RequestGroupSync
	ingester.HistoryQueued = conn.RequestHistory

	files := media.NewFiles(cfg.DataDir)
	ingester.RemoveFile = func(sha256 string) {
		if err := files.Remove(sha256); err != nil {
			logger.Warn("could not delete the file of a revoked message", logging.Err(err))
		}
	}
	worker := media.NewWorker(db, files, logger, cfg.MediaMaxBytes, cfg.MediaMaxPerHour)
	conn.SetMedia(worker)
	go worker.Sweep(ctx)
	go db.Listen(ctx, "media_requests", conn.RequestMedia)
	connDone := make(chan error, 1)
	go func() { connDone <- conn.Run(ctx) }()

	handler := httpapi.NewHandler(httpapi.Options{
		Logger:  logger,
		Service: "gateway",
		Version: version,
		Checks:  map[string]httpapi.Checker{"db": db},
		Info:    map[string]func() any{"whatsapp": func() any { return conn.Status() }},
	})
	logger.Info("gateway started", "addr", addr, "data_dir", cfg.DataDir)

	err = httpapi.Serve(ctx, httpapi.NewServer(handler, logger), ln, shutdownGrace)
	stop()
	if connErr := <-connDone; connErr != nil && err == nil {
		err = connErr
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	logger.Info("gateway stopped")
	return nil
}

// mediaCommand implements `gateway media fetch <message id>`. It only talks to the database: the
// running gateway, which holds the WhatsApp connection, does the download.
func mediaCommand(args []string) error {
	if len(args) != 2 || args[0] != "fetch" {
		return errors.New("usage: gateway media fetch <message id>")
	}
	messageID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil || messageID < 1 {
		return errors.New("the message id is the number in messages.id")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := store.Open(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	path, err := media.Fetch(ctx, db, media.NewFiles(cfg.DataDir), messageID, 2*time.Minute, time.Second)
	if err != nil {
		return err
	}
	fmt.Println(path)
	return nil
}

func pair(args []string) error {
	flags := flag.NewFlagSet("pair", flag.ContinueOnError)
	phone := flags.String("phone", "", "pair with a code for this number (international, digits only) instead of a QR code")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("pair takes no arguments, got %q", flags.Args())
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	logger := logging.New(os.Stderr, "warn", "service", "gateway", "version", version, "command", "pair")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	sqlDB := db.SQL()
	defer func() { _ = sqlDB.Close() }()

	validator, err := contracts.NewValidator()
	if err != nil {
		return err
	}
	ingester := ingest.NewIngester(db, ingest.NewStore(events.NewOutbox(validator)), logger)
	sessions := whatsapp.NewSessions(sqlDB, whatsapp.NewLogger(logger))
	err = whatsapp.Pair(ctx, sessions, ingester, logger, whatsapp.PairOptions{Phone: *phone, Out: os.Stdout})
	if errors.Is(err, context.Canceled) {
		return errors.New("pairing cancelled; nothing was linked")
	}
	return err
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
