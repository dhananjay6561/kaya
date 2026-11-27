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

	"github.com/jackc/pgx/v5"

	"github.com/dhananjay6561/kaya/contracts"
	"github.com/dhananjay6561/kaya/core/internal/actions"
	"github.com/dhananjay6561/kaya/core/internal/agent"
	"github.com/dhananjay6561/kaya/core/internal/alerts"
	"github.com/dhananjay6561/kaya/core/internal/channel"
	"github.com/dhananjay6561/kaya/core/internal/chat"
	"github.com/dhananjay6561/kaya/core/internal/commands"
	"github.com/dhananjay6561/kaya/core/internal/config"
	"github.com/dhananjay6561/kaya/core/internal/embeddings"
	"github.com/dhananjay6561/kaya/core/internal/facts"
	"github.com/dhananjay6561/kaya/core/internal/gateway"
	"github.com/dhananjay6561/kaya/core/internal/jobs"
	"github.com/dhananjay6561/kaya/core/internal/llm"
	"github.com/dhananjay6561/kaya/core/internal/masking"
	"github.com/dhananjay6561/kaya/core/internal/memory"
	"github.com/dhananjay6561/kaya/core/internal/pause"
	"github.com/dhananjay6561/kaya/core/internal/search"
	"github.com/dhananjay6561/kaya/core/internal/status"
	"github.com/dhananjay6561/kaya/core/internal/summary"
	"github.com/dhananjay6561/kaya/core/internal/tagging"
	"github.com/dhananjay6561/kaya/core/internal/tools"
	"github.com/dhananjay6561/kaya/core/internal/voice"
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
		case "search":
			exitOnError(searchChats(os.Args[2:]))
			return
		default:
			exitOnError(fmt.Errorf("unknown command %q; the commands are healthcheck, send and search", os.Args[1]))
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
	embedder, err := embeddings.New(cfg.Embeddings)
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
	indexer := search.NewIndexer(search.IndexerOptions{DB: db, Embeddings: embedder, Pause: switches, Logger: logger, Zone: zone})
	// Recall runs on every question, so its query gets a client that does not retry and a short
	// deadline: a slow embedding service costs a reply a few seconds, not minutes.
	queryEmbedder := embedder
	if cfg.Embeddings.Provider == "voyage" {
		queryEmbedder = embeddings.NewVoyage(cfg.Embeddings.Voyage, embeddings.VoyageOptions{Timeout: 4 * time.Second, MaxRetries: -1})
	}
	searcher := search.NewSearcher(db, queryEmbedder)
	factStore := facts.NewStore(db)
	learner, err := facts.NewLearner(facts.LearnerOptions{
		DB: db, LLM: model, Pause: switches, Logger: logger, Zone: zone,
		NewMasker: func() facts.Masker { return masking.NewSession() },
	})
	if err != nil {
		return err
	}
	tasks := tagging.NewTasks(db, time.Now)
	var tagger *tagging.Tagger
	if cfg.TaggingTokensPerDay > 0 {
		tagger, err = tagging.New(tagging.Options{
			DB: db, LLM: model, Pause: switches, Logger: logger, Zone: zone, TokensPerDay: cfg.TaggingTokensPerDay,
			NewMaskSession: func() tagging.MaskSession { return masking.NewSession() },
		})
		if err != nil {
			return err
		}
	}
	actionStore := actions.NewStore(db, time.Now, 10*time.Minute)
	jobStore := jobs.NewStore(db)
	toolset := []agent.Tool{
		tools.Clock(zone, time.Now), tools.OpenTasks(tasks, zone, time.Now),
		tools.SetReminder(jobStore, zone, time.Now), tools.ListReminders(jobStore, zone, time.Now), tools.CancelReminder(jobStore, zone, time.Now),
	}
	runner := &actions.Runner{
		Store: actionStore, Logger: logger, Executors: map[string]actions.Executor{}, MessageID: ownMessageID(db),
		Judge: actions.NewJudge(model, func() actions.Masker { return masking.NewSession() }, logger),
	}
	if cfg.SendsPerHour > 0 {
		toolset = append(toolset, tools.SendMessage(searcher, db))
		runner.Executors[tools.SendKind] = tools.SendExecutor(ch, actionStore, cfg.SendsPerHour, time.Now)
	}
	registry, err := agent.NewRegistry(toolset...)
	if err != nil {
		return err
	}
	summaries, err := summary.New(summary.Options{
		DB: db, LLM: model, Searcher: searcher, Zone: zone,
		NewMaskSession: func() summary.MaskSession { return masking.NewSession() },
	})
	if err != nil {
		return err
	}
	chatOptions := chat.Options{
		LLM:    model,
		Logger: logger,
		Zone:   zone,
		History: func(ctx context.Context, _ chat.Question) ([]memory.Exchange, error) {
			past, err := memory.ReadConversation(ctx, db, 0)
			if err != nil {
				return nil, err
			}
			sent, err := alerts.Recent(ctx, db, time.Now().Add(-12*time.Hour), 5)
			if err != nil {
				return nil, err
			}
			fired, err := jobs.Recent(ctx, db, time.Now().Add(-12*time.Hour), 5)
			return memory.Interleave(past, sent, fired), err
		},
		Recall: func(ctx context.Context, query string, exclude []int64, before time.Time) ([]memory.Exchange, error) {
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			return searcher.Recall(ctx, query, exclude, before, 10)
		},
		Excerpts: func(ctx context.Context, p search.Plan) (search.Found, error) {
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			return searcher.Excerpts(ctx, p, 3)
		},
		Facts: func(ctx context.Context) ([]string, error) {
			all, err := factStore.List(ctx)
			texts := make([]string, len(all))
			for i, f := range all {
				texts[i] = f.Text
			}
			return texts, err
		},
		DatabaseAway:   databaseAway,
		NewMaskSession: func() chat.MaskSession { return masking.NewSession() },
		OwnerName:      func(ctx context.Context) (string, error) { return search.OwnerName(ctx, db) },
		Tools:          registry,
		Propose:        actionStore.Propose,
		ActionTTL:      actionStore.TTL(),
	}
	replies, err := chat.New(chatOptions)
	if err != nil {
		return err
	}
	// Scheduled jobs run with no one watching, so they may read and remind but never draft an
	// action for DJ to confirm.
	unattended := chatOptions
	unattended.Propose = nil
	scheduled, err := chat.New(unattended)
	if err != nil {
		return err
	}
	alertSwitch := alerts.NewSwitch(db)
	var alerter *alerts.Alerter
	if cfg.AlertsPerHour > 0 {
		alerter, err = alerts.New(alerts.Options{
			DB: db, Channel: ch, Pause: switches, Logger: logger, Zone: zone, PerHour: cfg.AlertsPerHour,
		})
		if err != nil {
			return err
		}
	}
	scheduler, err := jobs.NewScheduler(jobs.Options{
		DB: db, Channel: ch, Pause: switches, Logger: logger, Zone: zone,
		Ask: func(ctx context.Context, text string) (string, error) {
			answer, err := scheduled.Reply(ctx, chat.Question{ID: "job", Text: text, SentAt: time.Now()})
			return answer.Text, err
		},
	})
	if err != nil {
		return err
	}
	var transcriber *voice.Worker
	if cfg.LLM.Provider == "groq" {
		var heard func()
		if waker, ok := ch.(interface{ Wake() }); ok {
			heard = waker.Wake
		}
		transcriber, err = voice.NewWorker(voice.Options{
			DB: db, Transcriber: voice.NewGroq(cfg.LLM.Groq.APIKey, cfg.TranscribeModel, ""), Pause: switches,
			Logger: logger, MediaDir: cfg.MediaDir, Heard: heard,
		})
		if err != nil {
			return err
		}
	}
	stopCommands := commands.Start(commands.Options{
		Channel: ch,
		Logger:  logger,
		Pause:   switches,
		Facts:   factStore,
		Tasks:   tasks,
		Zone:    zone,
		Summary: func(ctx context.Context, name string) (string, error) {
			if strings.TrimSpace(name) == "" {
				return summaries.Today(ctx)
			}
			return summaries.Chat(ctx, name)
		},
		Status: func(ctx context.Context) (string, error) {
			st, err := status.Read(ctx, status.Source{
				DB: db, Pause: switches, Version: version, StartedAt: startedAt,
				Index: func(ctx context.Context) (int, int, int, error) {
					p, err := indexer.Progress(ctx)
					return p.Indexed, p.Skipped, p.Total, err
				},
				Facts:  factStore.Count,
				Alerts: alertsStatus(alerter, alertSwitch),
				Tagging: func(ctx context.Context) (int, int, error) {
					open, err := tasks.CountOpen(ctx)
					if err != nil || tagger == nil {
						return 0, open, err
					}
					waiting, err := tagging.Waiting(ctx, db)
					return waiting, open, err
				},
			})
			if err != nil {
				return "", err
			}
			return status.Format(st), nil
		},
		Chat: func(ctx context.Context, m channel.IncomingMessage) (commands.ChatAnswer, error) {
			answer, err := replies.Reply(ctx, chat.Question{ID: m.ID, Text: m.Text, SentAt: m.SentAt})
			return commands.ChatAnswer{Text: answer.Text, Remember: answer.Remember, ActionID: answer.ActionID}, err
		},
		DatabaseAway: databaseAway,
		Alerts:       alertsCommand(alerter, alertSwitch),
		Decide: func(ctx context.Context, m channel.IncomingMessage) (string, bool, error) {
			return runner.Handle(ctx, m.ID, m.Text)
		},
		Replay: func(ctx context.Context, m channel.IncomingMessage) (string, bool, error) {
			return runner.Replay(ctx, m.ID)
		},
		Shown: func(ctx context.Context, actionID string, sent channel.SentMessage) error {
			shown, err := actionStore.Shown(ctx, actionID, sent.ID, sent.SentAt)
			if err == nil && !shown {
				logger.Warn("an action asked about was no longer a draft", "action", actionID)
			}
			return err
		},
	})
	defer stopCommands()

	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if err := db.Check(checkCtx); err != nil {
		logger.Warn("db not reachable at startup; health reports it until it is", logging.Err(err))
	}
	cancel()
	for kind, name := range map[string]string{"llm": model.Name(), "embeddings": embedder.Name(), "channel": ch.Name(), "telephony": cfg.Telephony} {
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
	indexer.Start()
	defer indexer.Stop()
	learner.Start()
	defer learner.Stop()
	if tagger != nil {
		tagger.Start()
		defer tagger.Stop()
	}
	if alerter != nil {
		alerter.Start()
		defer alerter.Stop()
	}
	scheduler.Start()
	defer scheduler.Stop()
	if transcriber != nil {
		transcriber.Start()
		defer transcriber.Stop()
	}

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
		"embeddings", embedder.Name(),
		"channel", ch.Name(),
		"telephony", cfg.Telephony,
		"tools", registry.Names(),
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

// searchChats prints what a search of the chats finds, for checking the index from a terminal.
func searchChats(args []string) error {
	query := strings.TrimSpace(strings.Join(args, " "))
	if query == "" {
		return errors.New(`usage: core search "what to look for"`)
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	zone, err := time.LoadLocation(cfg.TimeZone)
	if err != nil {
		return err
	}
	embedder, err := embeddings.New(cfg.Embeddings)
	if err != nil {
		return err
	}
	db, err := store.Open(cfg.DatabaseURL, store.WithAppName("kaya-core-search"))
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	hits, err := search.NewSearcher(db, embedder).Search(ctx, query, search.Filter{}, 8)
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		fmt.Println("Nothing found.")
	}
	for i, h := range hits {
		var how []string
		if h.Meaning {
			how = append(how, fmt.Sprintf("meaning %.2f", 1-h.Distance))
		}
		if h.Words {
			how = append(how, "words")
		}
		first := h.Lines[0].SentAt.In(zone)
		fmt.Printf("\n%d. %s, %s (%s)\n", i+1, h.Title, first.Format("2 Jan 2006 15:04"), strings.Join(how, ", "))
		for _, l := range h.Lines {
			fmt.Printf("   %s %s: %s\n", l.SentAt.In(zone).Format("15:04"), l.Sender, l.Text)
		}
	}
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

// alertsCommand is the switch /alerts uses, or nil when ALERTS_PER_HOUR turned alerts off.
func alertsCommand(alerter *alerts.Alerter, sw *alerts.Switch) commands.AlertSwitch {
	if alerter == nil {
		return nil
	}
	return sw
}

// alertsStatus reports alerts for /status, or nil to leave the line out when they are turned off.
func alertsStatus(alerter *alerts.Alerter, sw *alerts.Switch) func(context.Context) (bool, int, error) {
	if alerter == nil {
		return nil
	}
	return func(ctx context.Context) (bool, int, error) {
		on, err := sw.Enabled(ctx)
		if err != nil {
			return false, 0, err
		}
		waiting, err := sw.Waiting(ctx)
		return on, waiting, err
	}
}

// ownMessageID finds the row of a message DJ sent by its WhatsApp id.
func ownMessageID(db *store.DB) func(context.Context, string) (int64, error) {
	return func(ctx context.Context, waID string) (int64, error) {
		var id int64
		err := db.QueryRow(ctx, "SELECT id FROM messages WHERE wa_id = $1 AND from_me ORDER BY id DESC LIMIT 1", waID).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return id, err
	}
}
