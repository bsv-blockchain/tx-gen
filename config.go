package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	AdminToken            string
	Port                  string
	StatePath             string
	LogLevel              string
	LogFormat             string
	ArcadeBaseURL         string
	WOCBaseURL            string
	NumChains             int
	FanoutSize            int
	SustainFee            uint64
	MaxTPS                int
	BroadcastConcurrency  int
	BroadcastRetryMax     int
	HTTPTimeout           time.Duration
	SSEReconnectInitial   time.Duration
	SSEReconnectMax       time.Duration
	SlackWebhookURL       string
	SlackChannel          string
	Environment           string
	InstanceID            string
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

func getEnvUint64(key string, def uint64) uint64 {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.ParseUint(v, 10, 64); err == nil {
			return i
		}
	}
	return def
}

func getEnvDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func LoadConfig() (*Config, error) {
	c := &Config{
		AdminToken:            getEnv("ADMIN_TOKEN", ""),
		Port:                  getEnv("PORT", "8080"),
		StatePath:             getEnv("STATE_PATH", "./state.db"),
		LogLevel:              getEnv("LOG_LEVEL", "info"),
		LogFormat:             getEnv("LOG_FORMAT", "text"),
		ArcadeBaseURL:         getEnv("ARCADE_BASE_URL", "https://arcade-v2-us-1.bsvblockchain.tech"),
		WOCBaseURL:            getEnv("WOC_BASE_URL", "https://api.whatsonchain.com/v1/bsv/main"),
		NumChains:             getEnvInt("NUM_CHAINS", 10000),
		FanoutSize:            getEnvInt("FANOUT_SIZE", 100),
		SustainFee:            getEnvUint64("SUSTAIN_FEE", 1000),
		MaxTPS:                getEnvInt("MAX_TPS", 1000),
		BroadcastConcurrency:  getEnvInt("BROADCAST_CONCURRENCY", 256),
		BroadcastRetryMax:     getEnvInt("BROADCAST_RETRY_MAX", 5),
		HTTPTimeout:           getEnvDuration("HTTP_TIMEOUT", 30*time.Second),
		SSEReconnectInitial:   getEnvDuration("SSE_RECONNECT_INITIAL", 1*time.Second),
		SSEReconnectMax:       getEnvDuration("SSE_RECONNECT_MAX", 30*time.Second),
		SlackWebhookURL:       getEnv("SLACK_WEBHOOK_URL", ""),
		SlackChannel:          getEnv("SLACK_CHANNEL", "#alerts"),
		Environment:           getEnv("ENVIRONMENT", "dev"),
		InstanceID:            getEnv("INSTANCE_ID", "local"),
	}
	if c.AdminToken == "" {
		return nil, fmt.Errorf("ADMIN_TOKEN is required")
	}
	return c, nil
}

func (c *Config) String() string {
	redacted := *c
	if redacted.AdminToken != "" {
		redacted.AdminToken = "***"
	}
	if redacted.SlackWebhookURL != "" {
		redacted.SlackWebhookURL = "***"
	}
	b, _ := json.MarshalIndent(redacted, "", "  ")
	return string(b)
}

func (c *Config) MarshalJSON() ([]byte, error) {
	redacted := *c
	if redacted.AdminToken != "" {
		redacted.AdminToken = "***"
	}
	if redacted.SlackWebhookURL != "" {
		redacted.SlackWebhookURL = "***"
	}
	return json.Marshal(redacted)
}
