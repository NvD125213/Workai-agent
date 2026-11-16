package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	_ "notification-job-mb/docs"
	aiservice "notification-job-mb/internal/adapter/ai-service"
	"notification-job-mb/internal/adapter/telegram"
	"notification-job-mb/internal/config"
	"notification-job-mb/internal/database"
	"notification-job-mb/internal/handlers"
	"notification-job-mb/internal/jobs"
	"notification-job-mb/internal/mailer"
	"notification-job-mb/internal/repositories"
	"notification-job-mb/internal/routes"
	"notification-job-mb/internal/services"
)

// @title           Notification Job API
// @version         1.0
// @description     REST API cho hệ thống notification job (Gin + PostgreSQL).
// @host            localhost:8080
// @BasePath        /
// @securityDefinitions.apikey  BearerAuth
// @in                          header
// @name                        Authorization
// @description                 Nhập "Bearer <access_token>"
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

	if cfg.IsProduction() {
		gin.SetMode(gin.ReleaseMode)
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

	tokenService, err := services.NewTokenService(services.TokenConfig{
		AccessSecret: cfg.JWTAccessSecret,
		Issuer:       cfg.JWTIssuer,
		AccessTTL:    time.Duration(cfg.JWTAccessExpiresMinutes) * time.Minute,
		RefreshTTL:   time.Duration(cfg.RefreshTokenExpiresDays) * 24 * time.Hour,
	})
	if err != nil {
		return err
	}

	authService, err := services.NewAuthService(
		repositories.NewUserRepository(db),
		repositories.NewRefreshTokenRepository(db),
		services.NewPasswordService(),
		tokenService,
	)
	if err != nil {
		return err
	}

	sender, err := newMailSender(cfg)
	if err != nil {
		return err
	}
	mailQueue := mailer.NewQueue(sender, mailer.QueueConfig{})
	defer func() {
		// Runs after the HTTP server stopped, so nothing enqueues any more.
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := mailQueue.Shutdown(ctx); err != nil {
			log.Printf("mail queue shutdown: %v", err)
		}
	}()

	resetService, err := services.NewPasswordResetService(
		repositories.NewUserRepository(db),
		repositories.NewPasswordResetRepository(db),
		mailQueue,
		services.NewPasswordService(),
		cfg.JWTAccessSecret,
	)
	if err != nil {
		return err
	}

	aiClient := aiservice.NewClient(cfg.AI.ServiceURL, nil)
	jobRepo := repositories.NewJobRepository(db)
	profileRepo := repositories.NewProfileRepository(db)
	searchRepo := repositories.NewSearchRepository(db)
	analysisRepo := repositories.NewAnalysisRepository(db)

	// Lượt tìm / phân tích đang chạy dở khi process trước dừng sẽ không bao giờ xong: đánh dấu FAILED
	// để người dùng chạy lại được (index chỉ cho một lượt đang chạy mỗi người).
	if n, err := searchRepo.FailUnfinished(context.Background(), "server restarted during the search"); err != nil {
		return err
	} else if n > 0 {
		log.Printf("marked %d unfinished job search(es) as failed", n)
	}
	if n, err := analysisRepo.FailUnfinished(context.Background(), "server restarted during the analysis; analyze again"); err != nil {
		return err
	} else if n > 0 {
		log.Printf("marked %d unfinished CV analysis(es) as failed", n)
	}
	cvService := services.NewCVService(aiClient, profileRepo, analysisRepo)
	defer func() {
		// Runs after the HTTP server stopped, so no new analysis starts.
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := cvService.Shutdown(ctx); err != nil {
			log.Printf("cv analysis shutdown: %v", err)
		}
	}()
	findJobs := jobs.NewFindJobs(searchRepo, profileRepo, analysisRepo, services.NewJobService(jobRepo), jobRepo, aiClient,
		jobs.FindJobConfig{MaxPages: cfg.AI.FindJobMaxPages, MaxConcurrent: cfg.AI.FindJobMaxConcurrent,
			ScoreTimeout: cfg.AI.FindJobScoreTimeout})
	defer func() {
		// Runs after the HTTP server stopped, so no new search starts.
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := findJobs.Shutdown(ctx); err != nil {
			log.Printf("find job shutdown: %v", err)
		}
	}()

	var botHandler *handlers.BotHandler
	if cfg.Telegram.Enabled() {
		tg, err := telegram.NewClient(telegram.Config{
			BotToken: cfg.Telegram.BotToken,
			ChatID:   cfg.Telegram.ChatID,
			// Above the long-polling wait of getUpdates.
			Timeout: 60 * time.Second,
		}, nil)
		if err != nil {
			return err
		}
		botHandler = handlers.NewBotHandler(services.NewBotService(tg))

		// The bot runs here, not in its own process, to share cvService and
		// findJobs: one crawl rate limit per site and one shutdown. Only one
		// process may poll a bot token, so run a single API replica.
		bot := jobs.NewTelegramBot(tg, services.NewTelegramRegisterService(repositories.NewUserRepository(db)),
			cvService, findJobs, jobs.TelegramCVConfig{})
		botCtx, stopBot := context.WithCancel(context.Background())
		botDone := make(chan struct{})
		go func() {
			defer close(botDone)
			log.Println("telegram bot: polling for messages")
			bot.Run(botCtx)
		}()
		// Registered after the cvService and findJobs shutdowns, so it runs before them.
		defer func() {
			stopBot()
			<-botDone
		}()
	}

	engine := gin.New()
	engine.Use(gin.Logger(), gin.Recovery())
	if err := engine.SetTrustedProxies(cfg.HTTP.TrustedProxies); err != nil {
		return fmt.Errorf("set trusted proxies: %w", err)
	}

	routes.Register(engine, routes.Dependencies{
		Health:        handlers.NewHealthHandler(db),
		Auth:          handlers.NewAuthHandler(authService),
		PasswordReset: handlers.NewPasswordResetHandler(resetService),
		CV:            handlers.NewCVHandler(cvService),
		FindJob:       handlers.NewFindJobHandler(findJobs),
		Bot:           botHandler,
		TokenVerifier: tokenService,
	})

	srv := &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           engine,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("api listening on %s", cfg.HTTP.Addr)
		log.Printf("swagger docs: http://localhost%s/swagger/index.html", cfg.HTTP.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	log.Println("api shutting down")
	return srv.Shutdown(shutdownCtx)
}

func newMailSender(cfg *config.Config) (mailer.Sender, error) {
	if cfg.SMTP.Host == "" {
		if cfg.IsProduction() {
			return nil, errors.New("SMTP_HOST is required in production")
		}
		log.Println("SMTP_HOST not set: emails are logged, not sent (development only)")
		return mailer.Log{}, nil
	}
	return mailer.NewSMTP(mailer.Config{
		Host:     cfg.SMTP.Host,
		Port:     cfg.SMTP.Port,
		Username: cfg.SMTP.Username,
		Password: cfg.SMTP.Password,
		From:     cfg.SMTP.From,
	})
}
