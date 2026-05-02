package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net/http"
	"time"
)

// subscribeSSE connects to the arcade SSE stream and logs all events.
// Reconnects automatically on disconnect.
func subscribeSSE(ctx context.Context, apiKey string) {
	for {
		if ctx.Err() != nil {
			return
		}
		if err := connectSSE(ctx, apiKey); err != nil && ctx.Err() == nil {
			log.Printf("SSE disconnected: %v — reconnecting in 5s", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func connectSSE(ctx context.Context, apiKey string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", arcadeBase+"/sse", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("SSE %d", resp.StatusCode)
	}

	log.Printf("SSE connected to %s/sse", arcadeBase)
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if line := scanner.Text(); line != "" {
			log.Printf("SSE: %s", line)
		}
	}
	return scanner.Err()
}
