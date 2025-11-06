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

	coreda "github.com/evstack/ev-node/core/da"
	"github.com/evstack/ev-node/da/jsonrpc"
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
	client, err := jsonrpc.NewClient(ctx, logger, url, token, 0.0, 1.0, 1970176)
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
	// get blob IDs
	result, err := c.DA.GetIDs(ctx, daHeight, namespace)
	if err != nil {
		if strings.Contains(err.Error(), "blob: not found") {
			return nil, nil // No blobs at this height
		}
		if strings.Contains(err.Error(), "future") {
			return nil, fmt.Errorf("DA height %d is in the future", daHeight)
		}
		return nil, fmt.Errorf("failed to get blob IDs: %w", err)
	}

	if result == nil || len(result.IDs) == 0 {
		return nil, nil
	}

	// get actual blob data
	blobs, err := c.DA.Get(ctx, result.IDs, namespace)
	if err != nil {
		return nil, fmt.Errorf("failed to get blobs: %w", err)
	}

	return blobs, nil
}

// VerifyBlobAtHeight verifies that a specific blob exists at the given DA height
// by computing its commitment and checking if it matches any commitment at that height
func (c *Client) VerifyBlobAtHeight(ctx context.Context, blob []byte, daHeight uint64, namespace []byte) (bool, error) {
	if len(blob) == 0 {
		return true, nil // empty blobs are valid (nothing to verify)
	}

	// compute commitment for our blob
	commitments, err := c.DA.Commit(ctx, [][]byte{blob}, namespace)
	if err != nil {
		return false, fmt.Errorf("failed to compute commitment: %w", err)
	}

	if len(commitments) == 0 {
		return false, fmt.Errorf("no commitment generated")
	}

	commitment := commitments[0]
	c.logger.Debug().
		Str("our_commitment", fmt.Sprintf("%x", commitment)).
		Int("commitment_size", len(commitment)).
		Int("blob_size", len(blob)).
		Uint64("da_height", daHeight).
		Msg("computed commitment for blob")

	// get all IDs at the DA height
	result, err := c.DA.GetIDs(ctx, daHeight, namespace)
	if err != nil {
		// TODO: don't check string, use concrete error type
		if strings.Contains(err.Error(), "blob: not found") {
			c.logger.Debug().Uint64("da_height", daHeight).Msg("no blobs found at DA height")
			return false, nil // no blobs at this height
		}
		return false, fmt.Errorf("failed to get IDs: %w", err)
	}

	if result == nil || len(result.IDs) == 0 {
		c.logger.Debug().Uint64("da_height", daHeight).Msg("no IDs returned")
		return false, nil
	}

	c.logger.Debug().
		Int("num_ids", len(result.IDs)).
		Uint64("da_height", daHeight).
		Msg("checking commitments from Celestia")

	// check if our commitment matches any commitment at this height
	// ID format: height (8 bytes) + commitment
	for i, id := range result.IDs {
		_, cmt, err := coreda.SplitID(id)
		if err != nil {
			c.logger.Warn().Err(err).Msg("failed to split ID")
			continue
		}

		if equal := bytes.Equal(commitment, cmt); equal {
			c.logger.Info().
				Int("blob_index", i).
				Str("cmt", fmt.Sprintf("%x", cmt)).
				Msg("found matching commitment")
			return true, nil
		}
	}

	c.logger.Warn().
		Str("commitment", fmt.Sprintf("%x", commitment)).
		Int("checked_blobs", len(result.IDs)).
		Msg("no matching commitment found")
	return false, nil
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
