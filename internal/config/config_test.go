package config

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bsv-blockchain/tx-gen/internal/txbuilder"
)

const testWIF = "cNGwGSc7KRrTmdLUZ54fiSXWbhLNDc2Eg5zNucgQxyQCzuQ5YRDq"

// envOf returns a getenv function over a fixed set of variables.
func envOf(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

func requiredEnv() map[string]string {
	return map[string]string{"ADMIN_TOKEN": "secret", "INSTANCE_ID": "inst01"}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := load(envOf(requiredEnv()))
	require.NoError(t, err)

	assert.Equal(t, "secret", cfg.AdminToken)
	assert.Equal(t, "inst01", cfg.InstanceID)
	assert.Equal(t, DefaultPort, cfg.Port)
	assert.Equal(t, DefaultStatePath, cfg.StatePath)
	assert.Equal(t, DefaultLogLevel, cfg.LogLevel)
	assert.Equal(t, DefaultLogFormat, cfg.LogFormat)
	assert.Equal(t, DefaultArcadeBaseURL, cfg.ArcadeBaseURL)
	assert.Equal(t, DefaultWOCBaseURL, cfg.WOCBaseURL)
	assert.Equal(t, DefaultNumChains, cfg.NumChains)
	assert.Equal(t, DefaultFanoutSize, cfg.FanoutSize)
	assert.Equal(t, txbuilder.DefaultSustainFee, cfg.SustainFee)
	assert.Equal(t, DefaultMaxTPS, cfg.MaxTPS)
	assert.Equal(t, DefaultBroadcastConcurrency, cfg.BroadcastConcurrency)
	assert.Equal(t, DefaultBroadcastRetryMax, cfg.BroadcastRetryMax)
	assert.Equal(t, DefaultHTTPTimeout, cfg.HTTPTimeout)
	assert.Equal(t, DefaultSSEReconnectInitial, cfg.SSEReconnectMin)
	assert.Equal(t, DefaultSSEReconnectMax, cfg.SSEReconnectMax)
	assert.Equal(t, DefaultSlackChannel, cfg.SlackChannel)
	assert.Equal(t, DefaultEnvironment, cfg.Environment)
	assert.Empty(t, cfg.PrivateKey)
	assert.Empty(t, cfg.ArcadeCallbackToken)
	assert.Empty(t, cfg.SlackWebhookURL)
}

func TestLoadOverrides(t *testing.T) {
	env := requiredEnv()
	env["PORT"] = "9090"
	env["NUM_CHAINS"] = "1200"
	env["FANOUT_SIZE"] = " 35 "
	env["SUSTAIN_FEE"] = "9"
	env["HTTP_TIMEOUT"] = "5s"
	env["SSE_RECONNECT_MIN"] = "2s"
	env["SSE_RECONNECT_MAX"] = "1m"
	env["ARCADE_CALLBACK_TOKEN"] = " callback "

	cfg, err := load(envOf(env))
	require.NoError(t, err)
	assert.Equal(t, "9090", cfg.Port)
	assert.Equal(t, 1200, cfg.NumChains)
	assert.Equal(t, 35, cfg.FanoutSize)
	assert.Equal(t, uint64(9), cfg.SustainFee)
	assert.Equal(t, 5*time.Second, cfg.HTTPTimeout)
	assert.Equal(t, 2*time.Second, cfg.SSEReconnectMin)
	assert.Equal(t, time.Minute, cfg.SSEReconnectMax)
	assert.Equal(t, "callback", cfg.ArcadeCallbackToken)
}

func TestLoadAcceptsLegacySSEReconnectInitial(t *testing.T) {
	env := requiredEnv()
	env["SSE_RECONNECT_INITIAL"] = "3s"
	cfg, err := load(envOf(env))
	require.NoError(t, err)
	assert.Equal(t, 3*time.Second, cfg.SSEReconnectMin)

	env["SSE_RECONNECT_MIN"] = "4s"
	cfg, err = load(envOf(env))
	require.NoError(t, err)
	assert.Equal(t, 4*time.Second, cfg.SSEReconnectMin, "SSE_RECONNECT_MIN wins")
}

func TestLoadP2PKHDefaultFee(t *testing.T) {
	env := requiredEnv()
	env["PRIVATE_KEY"] = testWIF
	cfg, err := load(envOf(env))
	require.NoError(t, err)
	assert.Equal(t, txbuilder.DefaultP2PKHSustainFee, cfg.SustainFee)
}

func TestLoadRequiresAdminToken(t *testing.T) {
	_, err := load(envOf(map[string]string{"INSTANCE_ID": "inst01"}))
	require.ErrorIs(t, err, ErrAdminTokenRequired)
}

func TestLoadRequiresInstanceID(t *testing.T) {
	_, err := load(envOf(map[string]string{"ADMIN_TOKEN": "secret", "INSTANCE_ID": "  "}))
	require.ErrorIs(t, err, ErrInstanceIDRequired)
	assert.Contains(t, err.Error(), "INSTANCE_ID")
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	tests := map[string]string{
		"NUM_CHAINS":            "lots",
		"FANOUT_SIZE":           "0",
		"MAX_TPS":               "-5",
		"BROADCAST_CONCURRENCY": "1.5",
		"BROADCAST_RETRY_MAX":   "0",
		"SUSTAIN_FEE":           "0",
		"HTTP_TIMEOUT":          "30",
		"SSE_RECONNECT_MIN":     "-1s",
		"SSE_RECONNECT_MAX":     "soon",
		"LOG_LEVEL":             "verbose",
		"LOG_FORMAT":            "xml",
	}
	for key, value := range tests {
		t.Run(key, func(t *testing.T) {
			env := requiredEnv()
			env[key] = value
			_, err := load(envOf(env))
			require.ErrorIs(t, err, ErrInvalidValue)
			assert.Contains(t, err.Error(), key)
		})
	}
}

func TestLoadReportsEveryInvalidValue(t *testing.T) {
	env := requiredEnv()
	env["NUM_CHAINS"] = "x"
	env["FANOUT_SIZE"] = "y"
	_, err := load(envOf(env))
	require.ErrorIs(t, err, ErrInvalidValue)
	assert.Contains(t, err.Error(), "NUM_CHAINS")
	assert.Contains(t, err.Error(), "FANOUT_SIZE")
}

func TestLoadAcceptsReconnectMinAboveDefaultMax(t *testing.T) {
	env := requiredEnv()
	env["SSE_RECONNECT_MIN"] = "60s" // above the 30s default max; the subscriber raises the max
	cfg, err := load(envOf(env))
	require.NoError(t, err)
	assert.Equal(t, time.Minute, cfg.SSEReconnectMin)
}

func TestLoadNormalizesLogSettings(t *testing.T) {
	env := requiredEnv()
	env["LOG_LEVEL"] = "WARN"
	env["LOG_FORMAT"] = "Text"
	cfg, err := load(envOf(env))
	require.NoError(t, err)
	assert.Equal(t, "warn", cfg.LogLevel)
	assert.Equal(t, "text", cfg.LogFormat)
}

func TestLoadReadsProcessEnvironment(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "secret")
	t.Setenv("INSTANCE_ID", "inst01")
	t.Setenv("PORT", "8181")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "8181", cfg.Port)
}

func TestConfigRedactsSecrets(t *testing.T) {
	env := requiredEnv()
	env["PRIVATE_KEY"] = testWIF
	env["ARCADE_CALLBACK_TOKEN"] = "callback-secret"
	env["SLACK_WEBHOOK_URL"] = "https://hooks.slack.test/secret"
	cfg, err := load(envOf(env))
	require.NoError(t, err)

	encoded, err := json.Marshal(cfg)
	require.NoError(t, err)
	formatted := fmt.Sprintf("%s %v", cfg, cfg)
	for _, secret := range []string{"secret", testWIF, "callback-secret", "hooks.slack.test"} {
		assert.NotContains(t, string(encoded), secret, "JSON leaked a secret")
		assert.NotContains(t, formatted, secret, "String leaked a secret")
	}
	assert.Contains(t, string(encoded), `"adminToken":"***"`)
	assert.Contains(t, string(encoded), `"instanceId":"inst01"`)

	redacted := cfg.Redacted()
	assert.Equal(t, redactedValue, redacted.PrivateKey)
	assert.Equal(t, testWIF, cfg.PrivateKey, "Redacted must not modify the original")
}

func TestConfigRedactedLeavesEmptySecretsEmpty(t *testing.T) {
	cfg, err := load(envOf(requiredEnv()))
	require.NoError(t, err)
	redacted := cfg.Redacted()
	assert.Empty(t, redacted.PrivateKey)
	assert.Empty(t, redacted.SlackWebhookURL)
	assert.Equal(t, redactedValue, redacted.AdminToken)
}
