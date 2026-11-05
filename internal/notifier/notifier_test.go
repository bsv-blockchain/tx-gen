package notifier

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// webhook is a fake Slack endpoint that records every payload.
type webhook struct {
	mu       sync.Mutex
	payloads []webhookPayload
	status   int
}

func (w *webhook) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	var p webhookPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		rw.WriteHeader(http.StatusBadRequest)
		return
	}
	w.mu.Lock()
	w.payloads = append(w.payloads, p)
	status := w.status
	w.mu.Unlock()
	if status != 0 {
		rw.WriteHeader(status)
	}
}

func (w *webhook) received() []webhookPayload {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]webhookPayload(nil), w.payloads...)
}

func newTestNotifier(t *testing.T, hook *webhook, logs *bytes.Buffer) *Notifier {
	t.Helper()
	server := httptest.NewServer(hook)
	t.Cleanup(server.Close)
	return New(Options{
		WebhookURL:  server.URL,
		Channel:     "#alerts",
		Environment: "test",
		InstanceID:  "inst01",
		Logger:      slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
}

func TestReportReorgPostsToSlack(t *testing.T) {
	hook := &webhook{}
	var logs bytes.Buffer
	n := newTestNotifier(t, hook, &logs)

	n.ReportReorg(t.Context(), 900_000, 2, 9_600, "old", "new")
	payloads := hook.received()
	require.Len(t, payloads, 1)
	assert.Equal(t, "#alerts", payloads[0].Channel)
	assert.Equal(t, reorgUsername, payloads[0].Username)
	assert.Contains(t, payloads[0].Text, ":warning:")
	assert.Contains(t, payloads[0].Text, "height=900000 depth=2 chains=9600 tip old->new [test/inst01]")
	assert.Contains(t, logs.String(), "slack reorg notification sent")
}

func TestReportReorgFlagsDeepReorgs(t *testing.T) {
	hook := &webhook{}
	n := newTestNotifier(t, hook, &bytes.Buffer{})
	n.ReportReorg(t.Context(), 1, deepReorg+1, 0, "a", "b")
	require.Len(t, hook.received(), 1)
	assert.Contains(t, hook.received()[0].Text, ":rotating_light:")
}

func TestReportReorgDedupesByHeight(t *testing.T) {
	hook := &webhook{}
	n := newTestNotifier(t, hook, &bytes.Buffer{})
	clock := time.Unix(1_700_000_000, 0)
	n.now = func() time.Time { return clock }

	n.ReportReorg(t.Context(), 100, 1, 0, "a", "b")
	n.ReportReorg(t.Context(), 100, 1, 0, "a", "b")
	n.ReportReorg(t.Context(), 101, 1, 0, "a", "b")
	assert.Len(t, hook.received(), 2, "a repeat of the same height is suppressed")

	clock = clock.Add(dedupeWindow)
	n.ReportReorg(t.Context(), 100, 1, 0, "a", "b")
	assert.Len(t, hook.received(), 3, "the same height is sent again after the window")
	assert.Len(t, n.last, 1, "expired keys are pruned")
}

func TestReportReorgLogsDeliveryFailure(t *testing.T) {
	hook := &webhook{status: http.StatusForbidden}
	var logs bytes.Buffer
	n := newTestNotifier(t, hook, &logs)
	n.ReportReorg(t.Context(), 5, 1, 0, "a", "b")
	assert.Contains(t, logs.String(), "slack reorg notification failed")
	assert.Contains(t, logs.String(), "HTTP 403")
}

func TestReportReorgLogsTransportFailure(t *testing.T) {
	var logs bytes.Buffer
	n := New(Options{
		WebhookURL: "http://127.0.0.1:1/unreachable",
		Logger:     slog.New(slog.NewTextHandler(&logs, nil)),
	})
	n.ReportReorg(t.Context(), 5, 1, 0, "a", "b")
	assert.Contains(t, logs.String(), "slack reorg notification failed")
}

func TestPostOmitsEmptyChannel(t *testing.T) {
	encoded, err := json.Marshal(webhookPayload{Text: "hi", Username: "u"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"text":"hi","username":"u"}`, string(encoded))
}
