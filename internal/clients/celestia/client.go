package celestia

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	libshare "github.com/celestiaorg/go-square/v3/share"
	"github.com/evstack/ev-node/pkg/da/jsonrpc"
	evnode "github.com/evstack/ev-node/types/pb/evnode/v1"
	"github.com/rs/zerolog"
	"golang.org/x/sync/singleflight"
	"google.golang.org/protobuf/proto"
)

const (
	// defaultCacheTTL keeps per-DA-height lookups long enough for every block in an
	// ev-node batch to be verified against a single fetch.
	defaultCacheTTL = 2 * time.Minute
	// sharedFetchTimeout bounds a deduplicated fetch, which runs detached from the
	// cancellation of whichever caller started it.
	sharedFetchTimeout = 30 * time.Second
)

// Client is a small wrapper around the evnode celestia jsonrpc client.
type Client struct {
	*jsonrpc.Client
	logger zerolog.Logger
	url    string
	token  string

	// getAll defaults to Blob.GetAll; overridable in tests.
	getAll func(ctx context.Context, height uint64, namespaces []libshare.Namespace) ([]*jsonrpc.Blob, error)

	// simplify: plain maps swept on insert, no LRU. Memory is bounded by
	// cacheTTL × DA heights in flight; switch to golang-lru/expirable if that grows.
	cacheTTL   time.Duration
	mu         sync.Mutex
	timestamps map[uint64]cacheEntry[time.Time]
	blobs      map[blobKey]cacheEntry[[][]byte]
	inflight   singleflight.Group
}

type blobKey struct {
	height    uint64
	namespace string
}

type cacheEntry[V any] struct {
	value   V
	expires time.Time
}

func cacheGet[K comparable, V any](mu *sync.Mutex, m *map[K]cacheEntry[V], k K) (V, bool) {
	mu.Lock()
	defer mu.Unlock()
	e, ok := (*m)[k]
	if !ok || time.Now().After(e.expires) {
		var zero V
		return zero, false
	}
	return e.value, true
}

func cachePut[K comparable, V any](mu *sync.Mutex, m *map[K]cacheEntry[V], k K, v V, ttl time.Duration) {
	mu.Lock()
	defer mu.Unlock()
	now := time.Now()
	if *m == nil {
		*m = make(map[K]cacheEntry[V])
	}
	for key, e := range *m {
		if now.After(e.expires) {
			delete(*m, key)
		}
	}
	(*m)[k] = cacheEntry[V]{value: v, expires: now.Add(ttl)}
}

// shared runs fetch once per key across concurrent callers. The fetch is detached
// from ctx cancellation (bounded by sharedFetchTimeout) so one caller giving up
// does not fail the others; each caller still returns as soon as its own ctx is done.
func (c *Client) shared(ctx context.Context, key string, fetch func(context.Context) (any, error)) (any, error) {
	ch := c.inflight.DoChan(key, func() (any, error) {
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sharedFetchTimeout)
		defer cancel()
		return fetch(fetchCtx)
	})
	select {
	case r := <-ch:
		return r.Val, r.Err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func NewClient(ctx context.Context, url, token string, logger zerolog.Logger) (*Client, error) {
	// Use ev-node's DA client (which connects to celestia-node)
	client, err := jsonrpc.NewClient(ctx, url, token, "")
	if err != nil {
		return nil, fmt.Errorf("failed to create celestia client: %w", err)
	}

	return &Client{
		Client:   client,
		logger:   logger.With().Str("component", "celestia_client").Logger(),
		url:      url,
		token:    token,
		getAll:   client.Blob.GetAll,
		cacheTTL: defaultCacheTTL,
	}, nil
}

// GetBlobsAtHeight retrieves all blobs at a specific DA height and namespace.
// Non-empty results are memoized for cacheTTL; the returned slices are shared and
// must not be modified.
func (c *Client) GetBlobsAtHeight(ctx context.Context, daHeight uint64, namespace []byte) ([][]byte, error) {
	ns, err := libshare.NewNamespaceFromBytes(namespace)
	if err != nil {
		return nil, fmt.Errorf("invalid namespace: %w", err)
	}

	key := blobKey{height: daHeight, namespace: string(namespace)}
	if blobs, ok := cacheGet(&c.mu, &c.blobs, key); ok {
		return blobs, nil
	}

	v, err := c.shared(ctx, fmt.Sprintf("blobs/%d/%x", daHeight, namespace), func(ctx context.Context) (any, error) {
		blobs, err := c.fetchBlobs(ctx, daHeight, ns)
		if err == nil && len(blobs) > 0 {
			cachePut(&c.mu, &c.blobs, key, blobs, c.cacheTTL)
		}
		return blobs, err
	})
	if err != nil {
		return nil, err
	}
	return v.([][]byte), nil
}

func (c *Client) fetchBlobs(ctx context.Context, daHeight uint64, ns libshare.Namespace) ([][]byte, error) {
	blobs, err := c.getAll(ctx, daHeight, []libshare.Namespace{ns})
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

// VerifyHeaderBlobAtHeight verifies a header blob, accounting for the DAHeaderEnvelope
// wrapper. ev-node submits the signed header plus an envelope signature (field 4) whose
// bytes cannot be recomputed, so the published blob is matched by its signed-header
// content instead of by commitment. Legacy blobs without an envelope also match.
func (c *Client) VerifyHeaderBlobAtHeight(ctx context.Context, signedHeaderBlob []byte, daHeight uint64, namespace []byte) (bool, error) {
	return c.verifyBlobAtHeight(ctx, "header", signedHeaderBlob, daHeight, namespace, func(blob []byte) ([]byte, error) {
		var sh evnode.SignedHeader
		if err := proto.Unmarshal(blob, &sh); err != nil {
			return nil, err
		}
		// drop the envelope signature (field 4, unknown to SignedHeader) but keep unknown
		// fields nested inside the header, which are part of the signed bytes.
		sh.ProtoReflect().SetUnknown(nil)
		return proto.Marshal(&sh)
	})
}

// VerifyDataBlobAtHeight verifies a data blob, accounting for the SignedData wrapper
// ev-node submits Data wrapped in SignedData, but the Store API returns unwrapped Data
func (c *Client) VerifyDataBlobAtHeight(ctx context.Context, unwrappedDataBlob []byte, daHeight uint64, namespace []byte) (bool, error) {
	return c.verifyBlobAtHeight(ctx, "data", unwrappedDataBlob, daHeight, namespace, func(blob []byte) ([]byte, error) {
		var signedData evnode.SignedData
		if err := proto.Unmarshal(blob, &signedData); err != nil {
			return nil, err
		}
		if signedData.Data == nil {
			return nil, errors.New("SignedData has nil Data")
		}
		return proto.Marshal(signedData.Data)
	})
}

// verifyBlobAtHeight reports whether any blob in the namespace at daHeight, reduced
// by unwrap to the form the Store API returns, equals want.
func (c *Client) verifyBlobAtHeight(ctx context.Context, kind string, want []byte, daHeight uint64, namespace []byte, unwrap func([]byte) ([]byte, error)) (bool, error) {
	if len(want) == 0 {
		return true, nil
	}

	blobs, err := c.GetBlobsAtHeight(ctx, daHeight, namespace)
	if err != nil {
		return false, fmt.Errorf("failed to get blobs: %w", err)
	}

	for i, blob := range blobs {
		got, err := unwrap(blob)
		if err != nil {
			c.logger.Debug().Int("blob_index", i).Str("kind", kind).Err(err).Msg("failed to unwrap blob, skipping")
			continue
		}
		if bytes.Equal(got, want) {
			return true, nil
		}
	}

	c.logger.Warn().
		Str("kind", kind).
		Uint64("da_height", daHeight).
		Int("checked_blobs", len(blobs)).
		Msg("no matching blob found")
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

// GetBlockTimestamp retrieves the timestamp of a celestia da block at the given height.
// Successful lookups are memoized for cacheTTL.
func (c *Client) GetBlockTimestamp(ctx context.Context, daHeight uint64) (time.Time, error) {
	if ts, ok := cacheGet(&c.mu, &c.timestamps, daHeight); ok {
		return ts, nil
	}

	v, err := c.shared(ctx, fmt.Sprintf("timestamp/%d", daHeight), func(ctx context.Context) (any, error) {
		ts, err := c.fetchBlockTimestamp(ctx, daHeight)
		if err == nil {
			cachePut(&c.mu, &c.timestamps, daHeight, ts, c.cacheTTL)
		}
		return ts, err
	})
	if err != nil {
		return time.Time{}, err
	}
	return v.(time.Time), nil
}

func (c *Client) fetchBlockTimestamp(ctx context.Context, daHeight uint64) (time.Time, error) {
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
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

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
