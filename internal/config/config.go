// Package config loads settings from environment variables (TASKLANE_*).
package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	Listen        string
	DatabaseURL   string
	BlobDir       string
	MaxUploadMB   int64
	SessionTTL    time.Duration
	BaseURL       string
	SMTPHost      string
	SMTPPort      int
	SMTPFrom      string
	SMTPUser      string
	SMTPPass      string
	AdminEmail    string
	AdminPassword string
	PresenceTTL   time.Duration
}

func get(k, d string) string {
	if v := os.Getenv("TASKLANE_" + k); v != "" {
		return v
	}
	return d
}

func Load() Config {
	port, _ := strconv.Atoi(get("SMTP_PORT", "25"))
	mb, _ := strconv.ParseInt(get("MAX_UPLOAD_MB", "100"), 10, 64)
	return Config{
		Listen:        get("LISTEN", ":8080"),
		DatabaseURL:   get("DATABASE_URL", "postgres://tl:tl@localhost:55432/tasklane"),
		BlobDir:       get("BLOB_DIR", "./data/blobs"),
		MaxUploadMB:   mb,
		SessionTTL:    14 * 24 * time.Hour,
		BaseURL:       get("BASE_URL", "http://localhost:8080"),
		SMTPHost:      get("SMTP_HOST", ""),
		SMTPPort:      port,
		SMTPFrom:      get("SMTP_FROM", "tasklane@localhost"),
		SMTPUser:      get("SMTP_USER", ""),
		SMTPPass:      get("SMTP_PASS", ""),
		AdminEmail:    get("ADMIN_EMAIL", ""),
		AdminPassword: get("ADMIN_PASSWORD", ""),
		PresenceTTL:   30 * time.Second,
	}
}
