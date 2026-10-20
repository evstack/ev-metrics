package drift

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/evstack/ev-metrics/pkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func newBlockNumberServer(t *testing.T, height *atomic.Uint64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":"0x%x"}`, req.ID, height.Load())
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestExportMetrics_TimeSinceLastBlockOnlyResetsWhenHeightAdvances(t *testing.T) {
	var height atomic.Uint64
	height.Store(10)
	srv := newBlockNumberServer(t, &height)

	reg := prometheus.NewRegistry()
	m := metrics.NewWithRegistry("test", reg)
	e := NewMetricsExporter("chain", srv.URL, nil, 1, zerolog.Nop())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = e.ExportMetrics(ctx, m)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	timeSince := func() float64 {
		m.UpdateTimeSinceLastBlock()
		families, err := reg.Gather()
		require.NoError(t, err)
		for _, mf := range families {
			if mf.GetName() == "test_time_since_last_block_seconds" {
				return mf.GetMetric()[0].GetGauge().GetValue()
			}
		}
		t.Fatal("time_since_last_block_seconds not found")
		return 0
	}

	// polls at ~1s (height 10, first seen) and ~2s (height unchanged)
	time.Sleep(2500 * time.Millisecond)
	require.GreaterOrEqual(t, timeSince(), 1.2, "constant height must not reset the gauge")

	// poll at ~3s sees a lower height
	height.Store(5)
	time.Sleep(time.Second)
	require.GreaterOrEqual(t, timeSince(), 2.2, "decreasing height must not reset the gauge")

	// poll at ~4s sees a new height
	height.Store(11)
	time.Sleep(time.Second)
	require.Less(t, timeSince(), 1.0, "advancing height must reset the gauge")
}
