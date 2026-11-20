package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/maclovin/thehackerdispatch/internal/auth"
	"github.com/maclovin/thehackerdispatch/internal/config"
	"github.com/maclovin/thehackerdispatch/internal/curate"
	"github.com/maclovin/thehackerdispatch/internal/discuss"
	"github.com/maclovin/thehackerdispatch/internal/fetch"
	"github.com/maclovin/thehackerdispatch/internal/ingest"
	"github.com/maclovin/thehackerdispatch/internal/jobs"
	"github.com/maclovin/thehackerdispatch/internal/llm"
	"github.com/maclovin/thehackerdispatch/internal/notify"
	"github.com/maclovin/thehackerdispatch/internal/pacing"
	"github.com/maclovin/thehackerdispatch/internal/prompts"
	"github.com/maclovin/thehackerdispatch/internal/publish"
	"github.com/maclovin/thehackerdispatch/internal/reaper"
	"github.com/maclovin/thehackerdispatch/internal/secretbox"
	"github.com/maclovin/thehackerdispatch/internal/settings"
	"github.com/maclovin/thehackerdispatch/internal/sources"
	"github.com/maclovin/thehackerdispatch/internal/store"
	"github.com/maclovin/thehackerdispatch/internal/web"
)

var (
	version = "dev"
	commit  = "none"
)

const (
	// drainDelay lets Kubernetes take the pod out of the Service before connections are refused.
	drainDelay = 5 * time.Second

	// drainDelay + shutdownTimeout stays under Kubernetes' default 30s termination grace period.
	shutdownTimeout = 20 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args, os.Getenv, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout io.Writer) error {
	command, rest := "serve", args[1:]
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		command, rest = rest[0], rest[1:]
	}
	switch command {
	case "serve":
		return serve(ctx, rest, getenv, stdout)
	case "migrate":
		return migrate(ctx, getenv, stdout)
	case "master-password":
		return printMasterPassword(getenv, stdout)
	case "version":
		fmt.Fprintf(stdout, "thehackerdispatch %s (%s)\n", version, commit)
		return nil
	default:
		return fmt.Errorf("unknown command %q (want serve, migrate, master-password or version)", command)
	}
}

func serve(ctx context.Context, args []string, getenv func(string) string, stdout io.Writer) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	mode := flags.String("mode", "all", "what to run: all, web (HTTP only) or worker (background jobs only)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *mode != "all" && *mode != "web" && *mode != "worker" {
		return fmt.Errorf("invalid --mode %q (want all, web or worker)", *mode)
	}

	cfg, err := config.Load(getenv)
	if err != nil {
		return fmt.Errorf("configuration:\n%w", err)
	}
	logger := slog.New(slog.NewJSONHandler(stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	logger.Info("starting", "version", version, "commit", commit, "mode", *mode)

	pool, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := store.Migrate(ctx, pool, logger); err != nil {
		return err
	}
	if err := auth.EnsureMaster(ctx, pool, cfg.MasterPasswordFile, logger); err != nil {
		return err
	}

	settingsStore := settings.New(pool)
	authService, err := auth.NewService(pool, settingsStore, cfg.MasterPasswordFile, logger)
	if err != nil {
		return err
	}
	fetcher := fetch.New(fetch.Options{
		UserAgent: fmt.Sprintf("thehackerdispatch/%s (+https://github.com/maclovin/thehackerdispatch)", version),
	})
	box, err := secretbox.New(cfg.SecretsKey)
	if err != nil {
		return err
	}
	llmStore := llm.NewStore(pool, box, logger, nil)
	ingester := ingest.New(pool, fetcher, logger)
	promptStore := prompts.New(pool)
	curator := curate.New(pool, llmStore, settingsStore, promptStore, logger)
	publisher := publish.New(pool, llmStore, settingsStore, promptStore, fetcher, logger)
	pacer := pacing.New(pool, settingsStore, publisher, logger)
	notifier := notify.New(pool, settingsStore, notify.NewSlack(cfg.SlackBotToken), cfg.BaseURL, logger)
	if !notifier.Enabled() {
		logger.Info("Slack disabled: SLACK_BOT_TOKEN isn't set")
	}
	var sso *auth.SSO
	if cfg.OIDC.Enabled() {
		sso = auth.NewSSO(cfg.OIDC.IssuerURL, cfg.OIDC.ClientID, cfg.OIDC.ClientSecret, cfg.BaseURL.String()+"/auth/callback")
		logger.Info("SSO enabled", "issuer", cfg.OIDC.IssuerURL)
	}
	srv, err := web.NewServer(web.Deps{
		Logger:   logger,
		Pool:     pool,
		Settings: settingsStore,
		Auth:     authService,
		Sources:  sources.New(pool),
		Ingester: ingester,
		LLM:      llmStore,
		Prompts:  promptStore,
		Discuss:  discuss.New(pool),
		Notify:   notifier,
		SSO:      sso,
		Box:      box,
		BaseURL:  cfg.BaseURL,
		Build:    web.BuildInfo{Version: version, Commit: commit},
	})
	if err != nil {
		return err
	}
	handler := srv.HealthHandler()
	if *mode != "worker" {
		handler = srv.Handler()
	}
	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		logger.Info("http server listening", "addr", cfg.HTTPAddr)
		if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	})
	if *mode != "web" {
		for _, job := range []jobs.Job{
			{
				Name: ingest.JobName,
				Interval: func(ctx context.Context) time.Duration {
					s, err := settingsStore.Ingest(ctx)
					if err != nil {
						logger.Error("load ingest settings, using defaults", "err", err)
					}
					return s.PollInterval()
				},
				Run: func(ctx context.Context) error {
					stats, err := ingester.RunOnce(ctx)
					logger.Info("ingest cycle", "sources", stats.Sources, "failed", stats.Failed, "new_items", stats.NewItems)
					if err != nil {
						return err
					}
					cur, err := curator.RunOnce(ctx)
					logger.Info("curation", "items", cur.Items, "new_stories", cur.NewStories, "duplicates", cur.Duplicates,
						"updates", cur.Updates, "discarded", cur.Discarded, "retried", cur.Retried)
					if err != nil {
						return fmt.Errorf("curation: %w", err)
					}
					return jobs.RunSoon(ctx, pool, pacing.JobName)
				},
			},
			{
				Name:     pacing.JobName,
				Interval: func(context.Context) time.Duration { return time.Minute },
				Run: func(ctx context.Context) error {
					n, err := pacer.RunOnce(ctx)
					if n > 0 {
						logger.Info("published", "stories", n)
						if err := jobs.RunSoon(ctx, pool, notify.JobName); err != nil {
							logger.Warn("schedule Slack delivery", "err", err)
						}
					}
					return err
				},
			},
			{
				Name:     notify.JobName,
				Interval: func(context.Context) time.Duration { return time.Minute },
				Run: func(ctx context.Context) error {
					n, err := notifier.RunOnce(ctx)
					if n > 0 {
						logger.Info("Slack messages sent", "messages", n)
					}
					return err
				},
			},
			{
				Name:     "reaper",
				Interval: func(context.Context) time.Duration { return 5 * time.Minute },
				Run:      reaper.New(pool, logger).RunOnce,
			},
		} {
			g.Go(func() error { return jobs.Loop(gctx, pool, logger, job) })
		}
	}
	g.Go(func() error {
		<-gctx.Done()
		logger.Info("shutting down")
		srv.Drain()
		time.Sleep(drainDelay)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	})
	return g.Wait()
}

func migrate(ctx context.Context, getenv func(string) string, stdout io.Writer) error {
	url := getenv("DATABASE_URL")
	if url == "" {
		return errors.New("DATABASE_URL is required")
	}
	logger := slog.New(slog.NewJSONHandler(stdout, nil))
	pool, err := store.Open(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	return store.Migrate(ctx, pool, logger)
}

func printMasterPassword(getenv func(string) string, stdout io.Writer) error {
	path := getenv("MASTER_PASSWORD_FILE")
	if path == "" {
		path = config.DefaultMasterPasswordFile
	}
	password, err := auth.ReadMasterPassword(path)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "login:    %s\npassword: %s", auth.MasterNickname, password)
	return nil
}
