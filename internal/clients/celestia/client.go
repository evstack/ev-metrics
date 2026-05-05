package celestia

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	libshare "github.com/celestiaorg/go-square/v3/share"
	"github.com/evstack/ev-node/pkg/da/jsonrpc"
	evnode "github.com/evstack/ev-node/types/pb/evnode/v1"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/proto"
)

// Client is a small wrapper around the evnode celestia jsonrpc client.
type Client struct {
	*jsonrpc.Client
	logger zerolog.Logger
	url    string
}

func NewClient(ctx context.Context, url, token string, logger zerolog.Logger) (*Client, error) {
	// Use ev-node's DA client (which connects to celestia-node)
	client, err := jsonrpc.NewClient(ctx, url, token, "")
	if err != nil {
		return nil, fmt.Errorf("failed to create celestia client: %w", err)
	}

	return &Client{
		Client: client,
		logger: logger.With().Str("component", "celestia_client").Logger(),
		url:    url,
	}, nil
}

// GetBlobsAtHeight retrieves all blobs at a specific DA height and namespace
func (c *Client) GetBlobsAtHeight(ctx context.Context, daHeight uint64, namespace []byte) ([][]byte, error) {
	ns, err := libshare.NewNamespaceFromBytes(namespace)
	if err != nil {
		return nil, fmt.Errorf("invalid namespace: %w", err)
	}

	blobs, err := c.Blob.GetAll(ctx, daHeight, []libshare.Namespace{ns})
	if err != nil {
		if strings.Contains(err.Error(), "blob: not found") {
			return nil, nil
		}
		if strings.Contains(err.Error(), "future") {
			return nil, fmt.Errorf("DA height %d is in the future", daHeight)
		}
		return nil, fmt.Errorf("failed to get blobs: %w", err)
	}

	result := make([][]byte, len(blobs))
	for i, b := range blobs {
		result[i] = b.Data()
	}
	return result, nil
}

// VerifyBlobAtHeight verifies that a specific blob exists at the given DA height
// by computing its commitment and fetching it directly from Celestia.
func (c *Client) VerifyBlobAtHeight(ctx context.Context, blob []byte, daHeight uint64, namespace []byte) (bool, error) {
	if len(blob) == 0 {
		return true, nil // empty blobs are valid (nothing to verify)
	}

	ns, err := libshare.NewNamespaceFromBytes(namespace)
	if err != nil {
		return false, fmt.Errorf("invalid namespace: %w", err)
	}

	b, err := jsonrpc.NewBlobV0(ns, blob)
	if err != nil {
		return false, fmt.Errorf("failed to create blob for commitment: %w", err)
	}

	c.logger.Debug().
		Str("commitment", fmt.Sprintf("%x", b.Commitment)).
		Int("blob_size", len(blob)).
		Uint64("da_height", daHeight).
		Msg("fetching blob by commitment from Celestia")

	result, err := c.Blob.Get(ctx, daHeight, ns, b.Commitment)
	if err != nil {
		if strings.Contains(err.Error(), "blob: not found") {
			c.logger.Debug().Uint64("da_height", daHeight).Msg("blob not found at DA height")
			return false, nil
		}
		return false, fmt.Errorf("failed to get blob: %w", err)
	}

	if result != nil {
		c.logger.Info().
			Str("commitment", fmt.Sprintf("%x", b.Commitment)).
			Msg("blob verified on Celestia")
	}

	return result != nil, nil
}

// VerifyDataBlobAtHeight verifies a data blob, accounting for the SignedData wrapper
// ev-node submits Data wrapped in SignedData, but the Store API returns unwrapped Data
func (c *Client) VerifyDataBlobAtHeight(ctx context.Context, unwrappedDataBlob []byte, daHeight uint64, namespace []byte) (bool, error) {
	if len(unwrappedDataBlob) == 0 {
		return true, nil
	}

	c.logger.Debug().
		Int("unwrapped_size", len(unwrappedDataBlob)).
		Uint64("da_height", daHeight).
		Msg("verifying data blob (will check wrapped SignedData on Celestia)")

	// get all blobs at the DA height
	blobs, err := c.GetBlobsAtHeight(ctx, daHeight, namespace)
	if err != nil {
		return false, fmt.Errorf("failed to get blobs: %w", err)
	}

	if len(blobs) == 0 {
		c.logger.Debug().Uint64("da_height", daHeight).Msg("no blobs found")
		return false, nil
	}

	// try to unwrap each blob as SignedData and compare the inner Data
	for i, blob := range blobs {
		var signedData evnode.SignedData
		if err := proto.Unmarshal(blob, &signedData); err != nil {
			c.logger.Debug().
				Int("blob_index", i).
				Err(err).
				Msg("failed to unmarshal as SignedData, skipping")
			continue
		}

		if signedData.Data == nil {
			c.logger.Debug().Int("blob_index", i).Msg("SignedData has nil Data, skipping")
			continue
		}

		// marshal the inner Data to compare
		celestiaData, err := proto.Marshal(signedData.Data)
		if err != nil {
			c.logger.Debug().
				Int("blob_index", i).
				Err(err).
				Msg("failed to marshal inner Data")
			continue
		}

		equal := bytes.Equal(celestiaData, unwrappedDataBlob)

		c.logger.Debug().
			Int("blob_index", i).
			Int("celestia_data_size", len(celestiaData)).
			Int("evnode_data_size", len(unwrappedDataBlob)).
			Bool("matches", equal).
			Msg("comparing unwrapped Data")

		if equal {
			c.logger.Info().
				Int("blob_index", i).
				Msg("found matching Data")
			return true, nil
		}
	}

	c.logger.Warn().
		Int("checked_blobs", len(blobs)).
		Msg("no matching Data found in any SignedData wrapper")
	return false, nil
}

type Header struct {
	Time string `json:"time"`
}

type HeaderResult struct {
	Header Header `json:"header"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type RPCResponse struct {
	Result HeaderResult `json:"result"`
	Error  *RPCError    `json:"error"`
}

// GetBlockTimestamp retrieves the timestamp of a celestia da block at the given height
func (c *Client) GetBlockTimestamp(ctx context.Context, daHeight uint64) (time.Time, error) {
	payload := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "header.GetByHeight",
		"params":  []interface{}{daHeight},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to marshal json payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return time.Time{}, fmt.Errorf("http request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return time.Time{}, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to read response body: %w", err)
	}

	var rpcResp RPCResponse
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		return time.Time{}, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	if rpcResp.Error != nil {
		return time.Time{}, fmt.Errorf("rpc error (code %d): %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}

	timestamp, err := time.Parse(time.RFC3339Nano, rpcResp.Result.Header.Time)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to parse timestamp: %w", err)
	}

	c.logger.Debug().
		Uint64("da_height", daHeight).
		Time("timestamp", timestamp).
		Msg("retrieved da block timestamp")

	return timestamp, nil
}
