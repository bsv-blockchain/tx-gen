package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// BlockHeader matches chaintracks.BlockHeader + embedded block.Header JSON fields.
type BlockHeader struct {
	// From block.Header
	Version    int32  `json:"version"`
	PrevHash   string `json:"previousHash"`
	MerkleRoot string `json:"merkleRoot"`
	Timestamp  uint32 `json:"time"`
	Bits       uint32 `json:"bits"`
	Nonce      uint32 `json:"nonce"`
	// From chaintracks.BlockHeader
	Height uint32 `json:"height"`
	Hash   string `json:"hash"`
}

// ReorgEvent matches chaintracks.ReorgEvent.
type ReorgEvent struct {
	OrphanedHashes []string     `json:"orphanedHashes"`
	CommonAncestor *BlockHeader `json:"commonAncestor"`
	NewTip         *BlockHeader `json:"newTip"`
	Depth          uint32       `json:"depth"`
}

// subscribeSSE starts goroutines for both arcade SSE streams and reconnects on drop.
func subscribeSSE(ctx context.Context) {
	go streamLoop(ctx, arcadeBase+"/chaintracks/v2/tip/stream", "tip", func(data []byte) {
		var h BlockHeader
		if err := json.Unmarshal(data, &h); err != nil {
			log.Printf("SSE tip parse error: %v — raw: %s", err, data)
			return
		}
		log.Printf("SSE tip: height=%d hash=%s", h.Height, h.Hash)
	})

	go streamLoop(ctx, arcadeBase+"/chaintracks/v2/reorg/stream", "reorg", func(data []byte) {
		var r ReorgEvent
		if err := json.Unmarshal(data, &r); err != nil {
			log.Printf("SSE reorg parse error: %v — raw: %s", err, data)
			return
		}
		newHash := ""
		newHeight := uint32(0)
		if r.NewTip != nil {
			newHash = r.NewTip.Hash
			newHeight = r.NewTip.Height
		}
		log.Printf("SSE reorg: depth=%d orphans=%d newTip=%s@%d", r.Depth, len(r.OrphanedHashes), newHash, newHeight)
	})
}

// streamLoop connects to an SSE endpoint and dispatches each data payload to handler.
// Reconnects automatically on disconnect.
func streamLoop(ctx context.Context, url, name string, handler func([]byte)) {
	for {
		if ctx.Err() != nil {
			return
		}
		if err := connectStream(ctx, url, name, handler); err != nil && ctx.Err() == nil {
			log.Printf("SSE %s disconnected: %v — reconnecting in 5s", name, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func connectStream(ctx context.Context, url, name string, handler func([]byte)) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}

	log.Printf("SSE %s connected", name)
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "data: "):
			handler([]byte(strings.TrimPrefix(line, "data: ")))
		case strings.HasPrefix(line, ":"):
			// keepalive comment — ignore
		}
	}
	return scanner.Err()
}
