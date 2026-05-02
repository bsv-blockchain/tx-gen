package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const arcadeBase = "https://arcade-v2-us-1.bsvblockchain.tech"

type arcadeClient struct {
	http   *http.Client
	apiKey string
}

type arcadeResp struct {
	TXID        string `json:"txid"`
	Status      int    `json:"status"`
	Title       string `json:"title"`
	BlockHash   string `json:"blockHash"`
	BlockHeight int    `json:"blockHeight"`
	Timestamp   string `json:"timestamp"`
	ExtraInfo   string `json:"extraInfo"`
}

func newArcadeClient(apiKey string) *arcadeClient {
	return &arcadeClient{
		http:   &http.Client{Timeout: 30 * time.Second},
		apiKey: apiKey,
	}
}

func (c *arcadeClient) broadcast(efBytes []byte) (string, error) {
	req, err := http.NewRequest("POST", arcadeBase+"/tx", bytes.NewReader(efBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("arcade %d: %s", resp.StatusCode, body)
	}

	var result arcadeResp
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("decode response: %w — body: %s", err, body)
	}
	if result.TXID == "" {
		return "", fmt.Errorf("no txid in response: %s", body)
	}
	return result.TXID, nil
}
