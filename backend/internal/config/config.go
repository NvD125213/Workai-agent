package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	AppEnv string
	HTTP   HTTPConfig
	DB     DBConfig

	SMTP     SMTPConfig
	Telegram TelegramConfig

	JobImport JobImportConfig
	Crawl     CrawlConfig
	LinkedIn  LinkedInConfig
	AI        AIConfig

	JWTAccessSecret         string
	JWTIssuer               string
	JWTAccessExpiresMinutes int
	RefreshTokenExpiresDays int
}

type HTTPConfig struct {
	Addr string
	// TrustedProxies lists proxy IPs/CIDRs whose X-Forwarded-For is honored.
	// Empty means the direct peer address is always used as the client IP.
	TrustedProxies []string
}

// SMTPConfig is used by the API's background mail queue. An empty Host selects the
// development log mailer (rejected in production).
type SMTPConfig struct {
	Host     string
	Port     string
	Username string
	Password string
	From     string
}

// TelegramConfig is the bot the worker notifies through. An empty BotToken
// disables the notifications.
type TelegramConfig struct {
	BotToken string
	ChatID   string
}

func (c TelegramConfig) Enabled() bool {
	return c.BotToken != ""
}

// JobImportConfig is used by the worker to import the job files that the AI
// service writes into DataDir.
type JobImportConfig struct {
	DataDir      string
	PollInterval time.Duration
	MaxAttempts  int
}

// CrawlConfig drives the worker's scheduled run of the job-source adapters.
type CrawlConfig struct {
	Enabled    bool
	Keywords   []string
	Sources    []string // empty = every adapter
	Every      time.Duration
	MaxPages   int
	RunOnStart bool
}

// LinkedInConfig: the LinkedIn job source, used by the worker's crawl and the
// API's find-job. It is off by default because Vietnamese ISPs block LinkedIn:
// a blocked source would make every search wait for its retries.
type LinkedInConfig struct {
	Enabled bool
	// WireGuardConfig is the path of a wg-quick .conf file. When set, LinkedIn
	// requests (and their DNS lookups) go through that VPN, inside the process;
	// nothing else does. Empty = direct connection (e.g. a server abroad).
	WireGuardConfig string
}

// AIConfig: the internal AI service (llm/api.py) and the API's background
// job searches (POST /api/v1/find-job).
type AIConfig struct {
	ServiceURL           string
	FindJobMaxPages      int           // search pages per source
	FindJobMaxConcurrent int           // searches running at the same time
	FindJobScoreTimeout  time.Duration // max time for AI scoring of one search
}

type DBConfig struct {
	Host     string
	Port     string
	User     string
	Password string
	Name     string
	SSLMode  string
}

func (c DBConfig) DSN() string {
	return fmt.Sprintf(
		"host=%s user=%s password=%s dbname=%s port=%s sslmode=%s TimeZone=UTC",
		c.Host, c.User, c.Password, c.Name, c.Port, c.SSLMode,
	)
}

func Load() (*Config, error) {
	_ = godotenv.Load()

	cfg := &Config{
		AppEnv: getenv("APP_ENV", "development"),
		HTTP: HTTPConfig{
			Addr:           getenv("HTTP_ADDR", ":8080"),
			TrustedProxies: getenvList("TRUSTED_PROXIES"),
		},
		DB: DBConfig{
			Host:     getenv("DB_HOST", "localhost"),
			Port:     getenv("DB_PORT", "5432"),
			User:     getenv("DB_USER", "postgres"),
			Password: getenv("DB_PASSWORD", "postgres"),
			Name:     getenv("DB_NAME", "notification_job"),
			SSLMode:  getenv("DB_SSLMODE", "disable"),
		},

		SMTP: SMTPConfig{
			Host:     getenv("SMTP_HOST", ""),
			Port:     getenv("SMTP_PORT", "587"),
			Username: getenv("SMTP_USERNAME", ""),
			Password: getenv("SMTP_PASSWORD", ""),
			From:     getenv("SMTP_FROM", ""),
		},

		Telegram: TelegramConfig{
			BotToken: getenv("TELEGRAM_BOT_TOKEN", ""),
			ChatID:   getenv("TELEGRAM_CHAT_ID", ""),
		},

		JobImport: JobImportConfig{
			DataDir:      getenv("DATA_DIR", "data"),
			PollInterval: time.Duration(getenvInt("DATA_POLL_INTERVAL_SECONDS", 30)) * time.Second,
			MaxAttempts:  getenvInt("DATA_IMPORT_MAX_ATTEMPTS", 5),
		},

		Crawl: CrawlConfig{
			Enabled:    getenvBool("CRAWL_ENABLED", true),
			Keywords:   getenvList("CRAWL_KEYWORDS"),
			Sources:    getenvList("CRAWL_SOURCES"),
			Every:      time.Duration(getenvInt("CRAWL_INTERVAL_HOURS", 3)) * time.Hour,
			MaxPages:   getenvInt("CRAWL_MAX_PAGES", 5),
			RunOnStart: getenvBool("CRAWL_RUN_ON_START", false),
		},

		LinkedIn: LinkedInConfig{
			Enabled:         getenvBool("LINKEDIN_ENABLED", false),
			WireGuardConfig: getenv("LINKEDIN_WG_CONFIG", ""),
		},

		AI: AIConfig{
			ServiceURL:           getenv("AI_SERVICE_URL", "http://127.0.0.1:8001"),
			FindJobMaxPages:      getenvInt("FIND_JOB_MAX_PAGES", 3),
			FindJobMaxConcurrent: getenvInt("FIND_JOB_MAX_CONCURRENT", 3),
			FindJobScoreTimeout:  time.Duration(getenvInt("FIND_JOB_SCORE_TIMEOUT_MINUTES", 30)) * time.Minute,
		},

		JWTAccessSecret:         getenv("JWT_ACCESS_SECRET", ""),
		JWTIssuer:               getenv("JWT_ISSUER", "notification-job-mb"),
		JWTAccessExpiresMinutes: getenvInt("JWT_ACCESS_EXPIRES_MINUTES", 15),
		RefreshTokenExpiresDays: getenvInt("REFRESH_TOKEN_EXPIRES_DAYS", 7),
	}

	if cfg.HTTP.Addr == "" {
		return nil, fmt.Errorf("HTTP_ADDR is required")
	}
	if cfg.DB.Host == "" || cfg.DB.Name == "" {
		return nil, fmt.Errorf("DB_HOST and DB_NAME are required")
	}
	if cfg.Telegram.Enabled() && cfg.Telegram.ChatID == "" {
		return nil, fmt.Errorf("TELEGRAM_CHAT_ID is required when TELEGRAM_BOT_TOKEN is set")
	}

	return cfg, nil
}

func (c Config) IsProduction() bool {
	return c.AppEnv == "production"
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func getenvInt(key string, fallback int) int {
	valStr := os.Getenv(key)
	if valStr == "" {
		return fallback
	}

	val, err := strconv.Atoi(valStr)
	if err != nil {
		return fallback
	}

	return val
}

func getenvBool(key string, fallback bool) bool {
	v, err := strconv.ParseBool(os.Getenv(key))
	if err != nil {
		return fallback
	}
	return v
}

func getenvList(key string) []string {
	var out []string
	for _, v := range strings.Split(os.Getenv(key), ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
