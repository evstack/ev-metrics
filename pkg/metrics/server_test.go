package metrics

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

type failingExporter struct{ err error }

func (f failingExporter) ExportMetrics(context.Context, *Metrics) error { return f.err }

func TestStartServer_ExporterErrorStopsServer(t *testing.T) {
	l, err := net.Listen("tcp", ":0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())

	wantErr := errors.New("subscribe failed")
	done := make(chan error, 1)
	go func() {
		done <- StartServer(context.Background(), "/metrics", port, 0, zerolog.Nop(), failingExporter{err: wantErr})
	}()

	select {
	case err := <-done:
		require.ErrorIs(t, err, wantErr)
	case <-time.After(10 * time.Second):
		t.Fatal("StartServer did not return after exporter failure")
	}
}

func healthStatus(t *testing.T, m *Metrics, maxAge time.Duration) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	healthHandler(m, maxAge)(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	return rec.Code, rec.Body.String()
}

func TestHealthHandler(t *testing.T) {
	m := NewWithRegistry("test", prometheus.NewRegistry())

	code, body := healthStatus(t, m, 0)
	require.Equal(t, http.StatusServiceUnavailable, code)
	require.Contains(t, body, "not ready")

	m.SetSubscriptionUp(true)
	code, body = healthStatus(t, m, 0)
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, body, "OK")

	m.SetSubscriptionUp(false)
	code, body = healthStatus(t, m, 0)
	require.Equal(t, http.StatusServiceUnavailable, code)
	require.Contains(t, body, "down")

	m.SetSubscriptionUp(true)
	code, _ = healthStatus(t, m, 0)
	require.Equal(t, http.StatusOK, code)
}

func TestHealthHandlerHeaderAge(t *testing.T) {
	m := NewWithRegistry("test", prometheus.NewRegistry())
	m.SetSubscriptionUp(true)
	m.RecordHeaderReceived("chain", time.Now())
	time.Sleep(20 * time.Millisecond)

	// a stale header fails only when the age check is enabled
	code, _ := healthStatus(t, m, 0)
	require.Equal(t, http.StatusOK, code)
	code, body := healthStatus(t, m, time.Millisecond)
	require.Equal(t, http.StatusServiceUnavailable, code)
	require.Contains(t, body, "no block header received")

	m.RecordHeaderReceived("chain", time.Now())
	code, _ = healthStatus(t, m, time.Minute)
	require.Equal(t, http.StatusOK, code)
}

func TestHealthHandlerHeaderAgeBeforeFirstHeader(t *testing.T) {
	m := NewWithRegistry("test", prometheus.NewRegistry())
	m.SetSubscriptionUp(true)

	code, _ := healthStatus(t, m, time.Minute)
	require.Equal(t, http.StatusOK, code)

	time.Sleep(20 * time.Millisecond)
	code, _ = healthStatus(t, m, time.Millisecond)
	require.Equal(t, http.StatusServiceUnavailable, code)
}
