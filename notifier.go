package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

type Notifier struct {
	webhook  string
	channel  string
	env      string
	instance string
	mu       sync.Mutex
	last     map[string]time.Time
}

func NewNotifier(c *Config) *Notifier {
	return &Notifier{
		webhook:  c.SlackWebhookURL,
		channel:  c.SlackChannel,
		env:      c.Environment,
		instance: c.InstanceID,
		last:     make(map[string]time.Time),
	}
}

func (n *Notifier) shouldSend(key string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := time.Now()
	if last, ok := n.last[key]; ok && now.Sub(last) < 5*time.Minute {
		return false
	}
	n.last[key] = now
	return true
}

func (n *Notifier) post(msg string, isReorg bool) {
	if n.webhook == "" {
		return
	}
	payload := map[string]interface{}{
		"text":    msg,
		"channel": n.channel,
	}
	if isReorg {
		payload["username"] = "txgen-reorg"
	} else {
		payload["username"] = "txgen-alert"
	}
	b, _ := json.Marshal(payload)
	http.Post(n.webhook, "application/json", bytes.NewReader(b))
}

func (n *Notifier) logStructured(fields map[string]interface{}) {
	log.Printf("notifier: %+v", fields)
}

func (n *Notifier) ReportReorg(height, depth, chainsImpacted int, tipBefore, tipAfter string) {
	fields := map[string]interface{}{
		"event":           "reorg",
		"height":          height,
		"depth":           depth,
		"chains_impacted": chainsImpacted,
		"tip_before":      tipBefore,
		"tip_after":       tipAfter,
		"env":             n.env,
		"instance":        n.instance,
		"time":            time.Now().UTC(),
	}
	n.logStructured(fields)
	IncReorg(depth)
	if n.shouldSend(fmt.Sprintf("reorg-%d", height)) {
		emoji := ":warning:"
		if depth > 3 {
			emoji = ":rotating_light:"
		}
		msg := fmt.Sprintf("%s Reorg detected: height=%d depth=%d chains=%d tip %s->%s [%s]", emoji, height, depth, chainsImpacted, tipBefore, tipAfter, n.instance)
		n.post(msg, true)
	}
}

func (n *Notifier) AlertFailure(component, msg string) {
	key := "fail-" + component
	if !n.shouldSend(key) {
		return
	}
	fields := map[string]interface{}{"event": "failure", "component": component, "msg": msg, "env": n.env, "instance": n.instance}
	n.logStructured(fields)
	n.post(fmt.Sprintf(":x: Failure in %s: %s [%s]", component, msg, n.instance), false)
}

func (n *Notifier) AlertRecovery(component, msg string) {
	key := "recov-" + component
	fields := map[string]interface{}{"event": "recovery", "component": component, "msg": msg, "env": n.env, "instance": n.instance}
	n.logStructured(fields)
	if n.shouldSend(key) {
		n.post(fmt.Sprintf(":white_check_mark: Recovery in %s: %s [%s]", component, msg, n.instance), false)
	}
}
