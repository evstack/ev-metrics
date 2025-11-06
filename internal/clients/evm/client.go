package evm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/rs/zerolog"
)

// Client is a wrapper around the Ethereum client.
type Client struct {
	*ethclient.Client
	logger     zerolog.Logger
	rpcURL     string
	wsURL      string
	httpClient *http.Client
}

func NewClient(ctx context.Context, wsURL, rpcURL string, logger zerolog.Logger) (*Client, error) {
	client, err := ethclient.DialContext(ctx, wsURL)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to EVM client: %w", err)
	}

	return &Client{
		Client: client,
		logger: logger.With().Str("component", "evm_client").Logger(),
		rpcURL: rpcURL,
		wsURL:  wsURL,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}, nil
}

// HealthCheckRequest performs a lightweight JSON-RPC health check and returns the RTT duration
func (c *Client) HealthCheckRequest(ctx context.Context) (time.Duration, error) {
	// Create the JSON-RPC request for eth_blockNumber
	reqBody := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "eth_blockNumber",
		"id":      1,
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Measure RTT
	startTime := time.Now()

	req, err := http.NewRequestWithContext(ctx, "POST", c.rpcURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return 0, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("failed to send request: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			c.logger.Warn().Err(closeErr).Msg("failed to close response body")
		}
	}()

	duration := time.Since(startTime)

	// Read the response to ensure full RTT
	_, err = io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return duration, nil
}
