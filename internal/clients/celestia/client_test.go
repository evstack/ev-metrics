package celestia

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	libshare "github.com/celestiaorg/go-square/v3/share"
	"github.com/evstack/ev-node/pkg/da/jsonrpc"
	evnode "github.com/evstack/ev-node/types/pb/evnode/v1"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestGetBlockTimestamp_AuthorizationHeader(t *testing.T) {
	tests := []struct {
		name       string
		token      string
		wantHeader string
		wantSet    bool
	}{
		{name: "token configured", token: "secret", wantHeader: "Bearer secret", wantSet: true},
		{name: "no token", token: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotHeader []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotHeader = r.Header.Values("Authorization")
				_, _ = w.Write([]byte(`{"result":{"header":{"time":"2024-01-02T03:04:05Z"}}}`))
			}))
			defer srv.Close()

			c := &Client{logger: zerolog.Nop(), url: srv.URL, token: tt.token}
			_, err := c.GetBlockTimestamp(context.Background(), 1)
			require.NoError(t, err)

			if tt.wantSet {
				require.Equal(t, []string{tt.wantHeader}, gotHeader)
			} else {
				require.Empty(t, gotHeader)
			}
		})
	}
}

const okHeader = `{"result":{"header":{"time":"2024-01-02T03:04:05Z"}}}`

func newTimestampClient(t *testing.T, handler func(w http.ResponseWriter, call int64)) (*Client, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler(w, hits.Add(1))
	}))
	t.Cleanup(srv.Close)
	return &Client{logger: zerolog.Nop(), url: srv.URL, cacheTTL: time.Minute}, &hits
}

func TestGetBlockTimestamp_Cache(t *testing.T) {
	t.Run("same height fetched once", func(t *testing.T) {
		c, hits := newTimestampClient(t, func(w http.ResponseWriter, _ int64) {
			time.Sleep(20 * time.Millisecond)
			_, _ = w.Write([]byte(okHeader))
		})

		var wg sync.WaitGroup
		for range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := c.GetBlockTimestamp(context.Background(), 7)
				assert.NoError(t, err)
			}()
		}
		wg.Wait()
		for range 5 {
			ts, err := c.GetBlockTimestamp(context.Background(), 7)
			require.NoError(t, err)
			require.Equal(t, time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), ts)
		}
		require.EqualValues(t, 1, hits.Load())

		_, err := c.GetBlockTimestamp(context.Background(), 8)
		require.NoError(t, err)
		require.EqualValues(t, 2, hits.Load())
	})

	t.Run("http error not cached", func(t *testing.T) {
		c, hits := newTimestampClient(t, func(w http.ResponseWriter, call int64) {
			if call == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(okHeader))
		})
		_, err := c.GetBlockTimestamp(context.Background(), 1)
		require.Error(t, err)
		_, err = c.GetBlockTimestamp(context.Background(), 1)
		require.NoError(t, err)
		require.EqualValues(t, 2, hits.Load())
	})

	t.Run("rpc error not cached", func(t *testing.T) {
		c, hits := newTimestampClient(t, func(w http.ResponseWriter, call int64) {
			if call == 1 {
				_, _ = w.Write([]byte(`{"error":{"code":1,"message":"header: not found"}}`))
				return
			}
			_, _ = w.Write([]byte(okHeader))
		})
		_, err := c.GetBlockTimestamp(context.Background(), 1)
		require.Error(t, err)
		_, err = c.GetBlockTimestamp(context.Background(), 1)
		require.NoError(t, err)
		require.EqualValues(t, 2, hits.Load())
	})

	t.Run("expired entry refetched", func(t *testing.T) {
		c, hits := newTimestampClient(t, func(w http.ResponseWriter, _ int64) {
			_, _ = w.Write([]byte(okHeader))
		})
		c.cacheTTL = 10 * time.Millisecond
		_, err := c.GetBlockTimestamp(context.Background(), 1)
		require.NoError(t, err)
		time.Sleep(20 * time.Millisecond)
		_, err = c.GetBlockTimestamp(context.Background(), 1)
		require.NoError(t, err)
		require.EqualValues(t, 2, hits.Load())
	})

	t.Run("first caller cancel does not fail others", func(t *testing.T) {
		started, release := make(chan struct{}), make(chan struct{})
		c, hits := newTimestampClient(t, func(w http.ResponseWriter, _ int64) {
			close(started)
			<-release
			_, _ = w.Write([]byte(okHeader))
		})

		ctx, cancel := context.WithCancel(context.Background())
		firstErr := make(chan error, 1)
		go func() {
			_, err := c.GetBlockTimestamp(ctx, 1)
			firstErr <- err
		}()
		<-started

		secondErr := make(chan error, 1)
		go func() {
			_, err := c.GetBlockTimestamp(context.Background(), 1)
			secondErr <- err
		}()

		cancel()
		require.ErrorIs(t, <-firstErr, context.Canceled)
		close(release)
		require.NoError(t, <-secondErr)
		require.EqualValues(t, 1, hits.Load())
	})
}

func TestGetBlobsAtHeight_Cache(t *testing.T) {
	ns := libshare.MustNewV0Namespace([]byte("ev-metrics"))
	blob, err := jsonrpc.NewBlobV0(ns, []byte("payload"))
	require.NoError(t, err)

	newClient := func(results ...func() ([]*jsonrpc.Blob, error)) (*Client, *atomic.Int64) {
		var calls atomic.Int64
		return &Client{
			logger:   zerolog.Nop(),
			cacheTTL: time.Minute,
			getAll: func(context.Context, uint64, []libshare.Namespace) ([]*jsonrpc.Blob, error) {
				n := calls.Add(1)
				return results[min(int(n), len(results))-1]()
			},
		}, &calls
	}
	found := func() ([]*jsonrpc.Blob, error) { return []*jsonrpc.Blob{blob}, nil }

	t.Run("repeat calls fetch once", func(t *testing.T) {
		c, calls := newClient(found)
		for range 10 {
			blobs, err := c.GetBlobsAtHeight(context.Background(), 5, ns.Bytes())
			require.NoError(t, err)
			require.Equal(t, [][]byte{[]byte("payload")}, blobs)
		}
		require.EqualValues(t, 1, calls.Load())

		_, err := c.GetBlobsAtHeight(context.Background(), 6, ns.Bytes())
		require.NoError(t, err)
		require.EqualValues(t, 2, calls.Load())
	})

	tests := map[string]func() ([]*jsonrpc.Blob, error){
		"not found":   func() ([]*jsonrpc.Blob, error) { return nil, errors.New("blob: not found") },
		"empty":       func() ([]*jsonrpc.Blob, error) { return []*jsonrpc.Blob{}, nil },
		"other error": func() ([]*jsonrpc.Blob, error) { return nil, errors.New("connection refused") },
	}
	for name, first := range tests {
		t.Run(name+" not cached", func(t *testing.T) {
			c, calls := newClient(first, found)
			blobs, _ := c.GetBlobsAtHeight(context.Background(), 5, ns.Bytes())
			require.Empty(t, blobs)

			blobs, err := c.GetBlobsAtHeight(context.Background(), 5, ns.Bytes())
			require.NoError(t, err)
			require.Len(t, blobs, 1)
			require.EqualValues(t, 2, calls.Load())
		})
	}
}

func TestVerifyHeaderBlobAtHeight(t *testing.T) {
	ns := libshare.MustNewV0Namespace([]byte("ev-metrics"))
	signed := &evnode.SignedHeader{
		Header:    &evnode.Header{Height: 7, ChainId: "test"},
		Signature: []byte("sig"),
		Signer:    &evnode.Signer{Address: []byte("addr"), PubKey: []byte("pub")},
	}
	stored, err := proto.Marshal(signed)
	require.NoError(t, err)
	envelope, err := proto.Marshal(&evnode.DAHeaderEnvelope{
		Header: signed.Header, Signature: signed.Signature, Signer: signed.Signer,
		EnvelopeSignature: []byte("envelope-sig"),
	})
	require.NoError(t, err)
	require.NotEqual(t, stored, envelope)

	other, err := proto.Marshal(&evnode.DAHeaderEnvelope{Header: &evnode.Header{Height: 8}, EnvelopeSignature: []byte("x")})
	require.NoError(t, err)

	newClient := func(published ...[]byte) *Client {
		return &Client{
			logger:   zerolog.Nop(),
			cacheTTL: time.Minute,
			getAll: func(context.Context, uint64, []libshare.Namespace) ([]*jsonrpc.Blob, error) {
				var blobs []*jsonrpc.Blob
				for _, p := range published {
					b, err := jsonrpc.NewBlobV0(ns, p)
					require.NoError(t, err)
					blobs = append(blobs, b)
				}
				return blobs, nil
			},
		}
	}

	tests := map[string]struct {
		published [][]byte
		want      bool
	}{
		"envelope blob":          {[][]byte{other, envelope}, true},
		"legacy signed header":   {[][]byte{stored}, true},
		"different header":       {[][]byte{other}, false},
		"nothing at this height": {nil, false},
		"undecodable blob":       {[][]byte{{0xff, 0xff}, envelope}, true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := newClient(tt.published...).VerifyHeaderBlobAtHeight(context.Background(), stored, 5, ns.Bytes())
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}
