package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type wocClient struct {
	base string
	http *http.Client
}

func newWOCClient() *wocClient {
	return &wocClient{
		base: "https://api.whatsonchain.com/v1/bsv/main",
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

type wocUTXO struct {
	Height int64  `json:"height"`
	TxHash string `json:"tx_hash"`
	TxPos  uint32 `json:"tx_pos"`
	Value  uint64 `json:"value"`
}

func (c *wocClient) fetchUnspent(scriptHash string) ([]UTXO, error) {
	url := fmt.Sprintf("%s/script/%s/unspent/all", c.base, scriptHash)
	resp, err := c.http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("WOC %d: %s", resp.StatusCode, body)
	}

	var raw []wocUTXO
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	utxos := make([]UTXO, len(raw))
	for i, r := range raw {
		utxos[i] = UTXO{
			TxHash: r.TxHash,
			TxPos:  r.TxPos,
			Value:  r.Value,
			Height: r.Height,
		}
	}
	return utxos, nil
}

