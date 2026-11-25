// Command core is Kaya's agent: it reads what the gateway stores, answers DJ and acts for him.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/dhananjay6561/kaya/contracts"
	"github.com/dhananjay6561/kaya/core/internal/channel"
	"github.com/dhananjay6561/kaya/core/internal/chat"
	"github.com/dhananjay6561/kaya/core/internal/commands"
	"github.com/dhananjay6561/kaya/core/internal/config"
	"github.com/dhananjay6561/kaya/core/internal/gateway"
	"github.com/dhananjay6561/kaya/core/internal/llm"
	"github.com/dhananjay6561/kaya/core/internal/masking"
	"github.com/dhananjay6561/kaya/core/internal/memory"
	"github.com/dhananjay6561/kaya/core/internal/pause"
	"github.com/dhananjay6561/kaya/core/internal/status"
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
		case "send":
			exitOnError(send(os.Args[2:]))
			return
		default:
			exitOnError(fmt.Errorf("unknown command %q; the commands are healthcheck and send", os.Args[1]))
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
	startedAt := time.Now()
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
	validator, err := contracts.NewValidator()
	if err != nil {
		return err
	}
	zone, err := time.LoadLocation(cfg.TimeZone)
	if err != nil {
		return err
	}
	model, err := llm.New(cfg.LLM)
	if err != nil {
		return err
	}
	databaseAway := func(err error) bool { return channel.DatabaseAway(db, err) }

	events := gateway.NewEvents(logger)
	consumer := gateway.NewConsumer(gateway.ConsumerOptions{DB: db, Validator: validator, Logger: logger, Handle: events.Handle})
	var ch channel.Channel
	switch cfg.Channel {
	case "stub":
		ch = channel.NewStub()
	default:
		ch = channel.NewGateway(channel.GatewayOptions{
			DB: db, Validator: validator, Results: events, Incoming: events, Logger: logger,
			Inbox: channel.InboxOptions{IsUrgent: commands.IsBuiltin},
		})
	}
	switches := pause.NewStored(db)
	replies, err := chat.New(chat.Options{
		LLM:            model,
		Logger:         logger,
		Zone:           zone,
		History:        func(ctx context.Context, _ chat.Question) ([]memory.Exchange, error) { return memory.ReadConversation(ctx, db, 0) },
		DatabaseAway:   databaseAway,
		NewMaskSession: func() chat.MaskSession { return masking.NewSession() },
	})
	if err != nil {
		return err
	}
	stopCommands := commands.Start(commands.Options{
		Channel: ch,
		Logger:  logger,
		Pause:   switches,
		Status: func(ctx context.Context) (string, error) {
			st, err := status.Read(ctx, status.Source{DB: db, Pause: switches, Version: version, StartedAt: startedAt})
			if err != nil {
				return "", err
			}
			return status.Format(st), nil
		},
		Chat: func(ctx context.Context, m channel.IncomingMessage) (commands.ChatAnswer, error) {
			answer, err := replies.Reply(ctx, chat.Question{ID: m.ID, Text: m.Text, SentAt: m.SentAt})
			return commands.ChatAnswer{Text: answer.Text, Remember: answer.Remember}, err
		},
		DatabaseAway: databaseAway,
	})
	defer stopCommands()

	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if err := db.Check(checkCtx); err != nil {
		logger.Warn("db not reachable at startup; health reports it until it is", logging.Err(err))
	}
	cancel()
	for kind, name := range map[string]string{"llm": model.Name(), "channel": ch.Name(), "telephony": cfg.Telephony} {
		if name == "stub" {
			logger.Warn(kind+" provider is the stub; nothing real happens through it", "kind", kind)
		}
	}
	if err := ch.Start(ctx); err != nil {
		return err
	}
	defer ch.Stop()
	consumer.Start(ctx)
	defer consumer.Stop()

	ln, err := net.Listen("tcp", net.JoinHostPort(cfg.ListenHost, strconv.Itoa(cfg.Port)))
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	handler := httpapi.NewHandler(httpapi.Options{
		Logger:  logger,
		Service: "core",
		Version: version,
		Checks:  map[string]httpapi.Checker{"db": db, "channel": ch, "events": consumer},
	})
	logger.Info("core started",
		"addr", ln.Addr().String(),
		"llm", model.Name(),
		"channel", ch.Name(),
		"telephony", cfg.Telephony,
		"data_dir", cfg.DataDir,
	)
	err = httpapi.Serve(ctx, httpapi.NewServer(handler, logger), ln, shutdownGrace)
	logger.Info("core stopped")
	return err
}

// send puts one message in DJ's own chat through the gateway, for checking that sending works.
func send(args []string) error {
	text := strings.TrimSpace(os.Getenv("KAYA_TEXT"))
	if text == "" {
		text = strings.TrimSpace(strings.Join(args, " "))
	}
	if text == "" {
		return errors.New(`usage: make send text="hello"`)
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	db, err := store.Open(cfg.DatabaseURL, store.WithAppName("kaya-core-send"))
	if err != nil {
		return err
	}
	defer db.Close()
	validator, err := contracts.NewValidator()
	if err != nil {
		return err
	}
	ch := channel.NewGateway(channel.GatewayOptions{DB: db, Validator: validator, Poll: 500 * time.Millisecond, TTL: time.Minute})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := ch.Start(ctx); err != nil {
		return err
	}
	defer ch.Stop()
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	sent, err := ch.Send(ctx, channel.OutgoingMessage{IdempotencyKey: hex.EncodeToString(key), Text: text}, channel.SendOptions{})
	if err != nil {
		return err
	}
	fmt.Printf("sent to your own chat at %s (message %s)\n", sent.SentAt.UTC().Format(time.RFC3339Nano), sent.ID)
	return nil
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
