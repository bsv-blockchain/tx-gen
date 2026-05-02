package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"time"
)

const arcadeBase = "https://arcade-v2-us-1.bsvblockchain.tech"

type arcadeClient struct {
	http *http.Client
}

func newArcadeClient() *arcadeClient {
	return &arcadeClient{http: &http.Client{Timeout: 30 * time.Second}}
}

// broadcast sends an EF-encoded transaction to arcade.
// The txid must be computed by the caller via computeTxID before calling this.
func (c *arcadeClient) broadcast(efBytes []byte) error {
	req, err := http.NewRequest("POST", arcadeBase+"/tx", bytes.NewReader(efBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	// arcade returns 202 Accepted with {"status":"submitted"}
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("arcade %d: %s", resp.StatusCode, body)
	}
	return nil
}
