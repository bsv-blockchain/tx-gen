// Package config loads the tx-gen runtime configuration from environment
// variables.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bsv-blockchain/tx-gen/internal/txbuilder"
)

// Default values for optional settings.
const (
	DefaultPort                 = "8080"
	DefaultStatePath            = "./state.db"
	DefaultLogLevel             = "info"
	DefaultLogFormat            = "json"
	DefaultArcadeBaseURL        = "https://arcade-v2-us-1.bsvblockchain.tech"
	DefaultWOCBaseURL           = "https://api.whatsonchain.com/v1/bsv/main"
	DefaultNumChains            = 10_000
	DefaultFanoutSize           = 100
	DefaultMaxTPS               = 10_000
	DefaultBroadcastConcurrency = 64
	DefaultBroadcastRetryMax    = 3
	DefaultHTTPTimeout          = 30 * time.Second
	DefaultSSEReconnectInitial  = time.Second
	DefaultSSEReconnectMax      = 30 * time.Second
	DefaultSlackChannel         = "#alerts"
	DefaultEnvironment          = "production"
)

// redactedValue replaces every secret in logs and API responses.
const redactedValue = "***"

var (
	// ErrAdminTokenRequired is returned when ADMIN_TOKEN is not set.
	ErrAdminTokenRequired = errors.New("ADMIN_TOKEN is required")

	// ErrInstanceIDRequired is returned when INSTANCE_ID is not set.
	ErrInstanceIDRequired = errors.New("INSTANCE_ID is required")

	// ErrInvalidValue is returned when an environment variable is set to a
	// value that cannot be parsed or is out of range.
	ErrInvalidValue = errors.New("invalid configuration value")
)

// Config is the complete runtime configuration.
//
// Marshaling a Config to JSON, or formatting it with %s or %v, always redacts
// its secrets.
type Config struct {
	// AdminToken is the bearer token for the admin endpoints (secret).
	AdminToken string `json:"adminToken"`

	// Port is the HTTP listen port.
	Port string `json:"port"`

	// StatePath is the BoltDB state file.
	StatePath string `json:"statePath"`

	// LogLevel is debug, info, warn, or error.
	LogLevel string `json:"logLevel"`

	// LogFormat is json or text.
	LogFormat string `json:"logFormat"`

	// ArcadeBaseURL is the Arcade v2 node used for broadcasts and SSE.
	ArcadeBaseURL string `json:"arcadeBaseUrl"`

	// ArcadeCallbackToken enables the Arcade transaction-event stream (secret).
	ArcadeCallbackToken string `json:"arcadeCallbackToken"`

	// WOCBaseURL is the WhatsOnChain API used for UTXO lookups.
	WOCBaseURL string `json:"wocBaseUrl"`

	// PrivateKey switches to P2PKH mode when set (secret).
	PrivateKey string `json:"privateKey"`

	// InstanceID tags logs and alerts, and the tagged-drop lock script.
	InstanceID string `json:"instanceId"`

	// Environment tags logs and Slack messages.
	Environment string `json:"environment"`

	// NumChains is the number of chains bootstrap creates.
	NumChains int `json:"numChains"`

	// FanoutSize is the maximum outputs per bootstrap fanout transaction.
	FanoutSize int `json:"fanoutSize"`

	// SustainFee is the fee per sustain hop in satoshis.
	SustainFee uint64 `json:"sustainFee"`

	// MaxTPS is the highest TPS that POST /config accepts.
	MaxTPS int `json:"maxTps"`

	// BroadcastConcurrency caps in-flight Arcade broadcasts.
	BroadcastConcurrency int `json:"broadcastConcurrency"`

	// BroadcastRetryMax is the number of attempts per Arcade or WhatsOnChain
	// request.
	BroadcastRetryMax int `json:"broadcastRetryMax"`

	// HTTPTimeout bounds each Arcade and WhatsOnChain request.
	HTTPTimeout time.Duration `json:"httpTimeout"`

	// SSEReconnectMin is the first SSE reconnect delay.
	SSEReconnectMin time.Duration `json:"sseReconnectMin"`

	// SSEReconnectMax caps the SSE reconnect delay; a value below
	// SSEReconnectMin is raised to it.
	SSEReconnectMax time.Duration `json:"sseReconnectMax"`

	// SlackWebhookURL enables Slack reorg notifications (secret).
	SlackWebhookURL string `json:"slackWebhookUrl"`

	// SlackChannel is the Slack channel for notifications.
	SlackChannel string `json:"slackChannel"`
}

// Load reads the configuration from the environment.
//
// ADMIN_TOKEN and INSTANCE_ID are required. Unset or empty variables take
// their defaults; a variable that is set but cannot be parsed is an error, so
// a typo never silently falls back to a default.
func Load() (*Config, error) {
	return load(os.Getenv)
}

// Redacted returns a copy of c with every secret replaced by "***".
func (c *Config) Redacted() Config {
	redacted := *c
	for _, secret := range []*string{
		&redacted.AdminToken,
		&redacted.PrivateKey,
		&redacted.ArcadeCallbackToken,
		&redacted.SlackWebhookURL,
	} {
		if *secret != "" {
			*secret = redactedValue
		}
	}
	return redacted
}

// MarshalJSON encodes the redacted configuration.
func (c *Config) MarshalJSON() ([]byte, error) {
	type plain Config // drops the methods, so Marshal does not recurse
	redacted := plain(c.Redacted())
	return json.Marshal(&redacted)
}

// String returns the redacted configuration as indented JSON.
func (c *Config) String() string {
	encoded, err := c.MarshalJSON()
	if err != nil {
		return fmt.Sprintf("config: %v", err)
	}
	var indented bytes.Buffer
	if err = json.Indent(&indented, encoded, "", "  "); err != nil {
		return string(encoded)
	}
	return indented.String()
}

// load builds a Config from getenv, which returns "" for unset variables.
func load(getenv func(string) string) (*Config, error) {
	env := reader{getenv: getenv}

	privateKey := env.string("PRIVATE_KEY", "")
	defaultFee := txbuilder.DefaultSustainFee
	if privateKey != "" {
		defaultFee = txbuilder.DefaultP2PKHSustainFee
	}

	c := &Config{
		AdminToken:           env.string("ADMIN_TOKEN", ""),
		Port:                 env.string("PORT", DefaultPort),
		StatePath:            env.string("STATE_PATH", DefaultStatePath),
		LogLevel:             env.oneOf("LOG_LEVEL", DefaultLogLevel, "debug", "info", "warn", "error"),
		LogFormat:            env.oneOf("LOG_FORMAT", DefaultLogFormat, "json", "text"),
		ArcadeBaseURL:        env.string("ARCADE_BASE_URL", DefaultArcadeBaseURL),
		ArcadeCallbackToken:  env.string("ARCADE_CALLBACK_TOKEN", ""),
		WOCBaseURL:           env.string("WOC_BASE_URL", DefaultWOCBaseURL),
		PrivateKey:           privateKey,
		InstanceID:           env.string("INSTANCE_ID", ""),
		Environment:          env.string("ENVIRONMENT", DefaultEnvironment),
		NumChains:            env.positiveInt("NUM_CHAINS", DefaultNumChains),
		FanoutSize:           env.positiveInt("FANOUT_SIZE", DefaultFanoutSize),
		SustainFee:           env.positiveUint64("SUSTAIN_FEE", defaultFee),
		MaxTPS:               env.positiveInt("MAX_TPS", DefaultMaxTPS),
		BroadcastConcurrency: env.positiveInt("BROADCAST_CONCURRENCY", DefaultBroadcastConcurrency),
		BroadcastRetryMax:    env.positiveInt("BROADCAST_RETRY_MAX", DefaultBroadcastRetryMax),
		HTTPTimeout:          env.duration("HTTP_TIMEOUT", DefaultHTTPTimeout),
		// SSE_RECONNECT_INITIAL is the older name of SSE_RECONNECT_MIN.
		SSEReconnectMin: env.duration("SSE_RECONNECT_MIN", env.duration("SSE_RECONNECT_INITIAL", DefaultSSEReconnectInitial)),
		SSEReconnectMax: env.duration("SSE_RECONNECT_MAX", DefaultSSEReconnectMax),
		SlackWebhookURL: env.string("SLACK_WEBHOOK_URL", ""),
		SlackChannel:    env.string("SLACK_CHANNEL", DefaultSlackChannel),
	}
	if err := errors.Join(env.errs...); err != nil {
		return nil, err
	}
	if c.AdminToken == "" {
		return nil, ErrAdminTokenRequired
	}
	if c.InstanceID == "" {
		return nil, ErrInstanceIDRequired
	}
	return c, nil
}

// reader parses environment variables and collects every parse error, so
// that one run reports all of them at once.
type reader struct {
	getenv func(string) string
	errs   []error
}

// string returns the trimmed value of key, or def when it is empty.
func (r *reader) string(key, def string) string {
	if v := strings.TrimSpace(r.getenv(key)); v != "" {
		return v
	}
	return def
}

// oneOf returns key lowercased when it is one of allowed, and def when unset.
func (r *reader) oneOf(key, def string, allowed ...string) string {
	raw := r.string(key, "")
	if raw == "" {
		return def
	}
	value := strings.ToLower(raw)
	if !slices.Contains(allowed, value) {
		r.errs = append(r.errs, fmt.Errorf("%w: %s=%q must be one of %s", ErrInvalidValue, key, raw, strings.Join(allowed, ", ")))
		return def
	}
	return value
}

// positiveInt returns key parsed as an integer greater than zero.
func (r *reader) positiveInt(key string, def int) int {
	raw := r.string(key, "")
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		r.errs = append(r.errs, fmt.Errorf("%w: %s=%q must be a positive integer", ErrInvalidValue, key, raw))
		return def
	}
	return v
}

// positiveUint64 returns key parsed as an unsigned integer greater than zero.
func (r *reader) positiveUint64(key string, def uint64) uint64 {
	raw := r.string(key, "")
	if raw == "" {
		return def
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || v == 0 {
		r.errs = append(r.errs, fmt.Errorf("%w: %s=%q must be a positive integer", ErrInvalidValue, key, raw))
		return def
	}
	return v
}

// duration returns key parsed as a positive Go duration such as "30s".
func (r *reader) duration(key string, def time.Duration) time.Duration {
	raw := r.string(key, "")
	if raw == "" {
		return def
	}
	v, err := time.ParseDuration(raw)
	if err != nil || v <= 0 {
		r.errs = append(r.errs, fmt.Errorf("%w: %s=%q must be a positive duration such as 30s", ErrInvalidValue, key, raw))
		return def
	}
	return v
}
