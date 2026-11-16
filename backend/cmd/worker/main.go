package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"notification-job-mb/internal/adapter/telegram"
	"notification-job-mb/internal/config"
	"notification-job-mb/internal/database"
	"notification-job-mb/internal/jobs"
	"notification-job-mb/internal/repositories"
	"notification-job-mb/internal/services"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := jobs.ConfigureSites(jobs.SiteOptions{
		LinkedIn: cfg.LinkedIn.Enabled, LinkedInWireGuardConfig: cfg.LinkedIn.WireGuardConfig,
	}); err != nil {
		return err
	}

	db, err := database.Connect(cfg.DB)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := database.Close(db); cerr != nil {
			log.Printf("close database: %v", cerr)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	jobImport, err := jobs.NewJobImport(
		services.NewJobService(repositories.NewJobRepository(db)),
		jobs.JobImportConfig{
			DataDir:      cfg.JobImport.DataDir,
			PollInterval: cfg.JobImport.PollInterval,
			MaxAttempts:  cfg.JobImport.MaxAttempts,
		},
	)
	if err != nil {
		return err
	}

	var notifier *telegram.Client
	if cfg.Telegram.Enabled() {
		notifier, err = telegram.NewClient(telegram.Config{
			BotToken: cfg.Telegram.BotToken,
			ChatID:   cfg.Telegram.ChatID,
		}, nil)
		if err != nil {
			return err
		}
		// A wrong token or chat must not stop the worker; it only loses notifications.
		if err := notifier.Ping(ctx); err != nil {
			log.Printf("telegram: connection check failed: %v", err)
		} else {
			log.Println("telegram: connected")
		}
	}
	_ = notifier // TODO: pass to the jobs that send notifications

	var autoCrawl *jobs.AutoCrawl
	if cfg.Crawl.Enabled {
		autoCrawl, err = jobs.NewAutoCrawl(repositories.NewJobRepository(db), jobs.AutoCrawlConfig{
			DataDir:    cfg.JobImport.DataDir,
			Keywords:   cfg.Crawl.Keywords,
			Sources:    cfg.Crawl.Sources,
			Every:      cfg.Crawl.Every,
			MaxPages:   cfg.Crawl.MaxPages,
			RunOnStart: cfg.Crawl.RunOnStart,
		})
		if err != nil {
			return err
		}
	}

	log.Printf("worker started (env=%s)", cfg.AppEnv)
	// Each Run returns once ctx is cancelled and its current step is finished,
	// so the deferred database close happens after them.
	var wg sync.WaitGroup
	wg.Go(func() { jobImport.Run(ctx) })
	if autoCrawl != nil {
		wg.Go(func() { autoCrawl.Run(ctx) })
	}
	wg.Wait()
	log.Println("worker shutting down")
	return nil
}
