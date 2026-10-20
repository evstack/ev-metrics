package evm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func newTestClient(srv *httptest.Server) *Client {
	return &Client{rpcURL: srv.URL, httpClient: srv.Client(), logger: zerolog.Nop()}
}

func TestHealthCheckRequest_SuccessMeasuresFullBody(t *testing.T) {
	const delay = 100 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,`))
		w.(http.Flusher).Flush()
		time.Sleep(delay)
		_, _ = w.Write([]byte(`"result":"0x10"}`))
	}))
	defer srv.Close()

	d, err := newTestClient(srv).HealthCheckRequest(context.Background())
	require.NoError(t, err)
	require.GreaterOrEqual(t, d, delay)
}

func TestHealthCheckRequest_UnexpectedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := newTestClient(srv).HealthCheckRequest(context.Background())
	require.ErrorContains(t, err, "unexpected status code: 500")
}

func TestHealthCheckRequest_JSONRPCError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`))
	}))
	defer srv.Close()

	_, err := newTestClient(srv).HealthCheckRequest(context.Background())
	require.ErrorContains(t, err, "-32601")
	require.ErrorContains(t, err, "method not found")
}
