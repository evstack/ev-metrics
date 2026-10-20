package drift

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

	// polls at ~0s (height 10, first seen), ~1s and ~2s (height unchanged)
	time.Sleep(2500 * time.Millisecond)
	require.GreaterOrEqual(t, timeSince(), 2.2, "constant height must not reset the gauge")

	// poll at ~3s sees a lower height
	height.Store(5)
	time.Sleep(time.Second)
	require.GreaterOrEqual(t, timeSince(), 3.2, "decreasing height must not reset the gauge")

	// poll at ~4s sees a new height
	height.Store(11)
	time.Sleep(time.Second)
	require.Less(t, timeSince(), 1.0, "advancing height must reset the gauge")
}

func gaugeValue(t *testing.T, reg *prometheus.Registry, name, endpoint string) (float64, bool) {
	t.Helper()
	families, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, metric := range mf.GetMetric() {
			for _, l := range metric.GetLabel() {
				if l.GetValue() == endpoint {
					return metric.GetGauge().GetValue(), true
				}
			}
		}
	}
	return 0, false
}

func TestExportMetrics_HungFullNodeDoesNotBlockOthers(t *testing.T) {
	var refHeight, goodHeight atomic.Uint64
	refHeight.Store(100)
	goodHeight.Store(90)
	ref := newBlockNumberServer(t, &refHeight)
	good := newBlockNumberServer(t, &goodHeight)
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// the body must be drained for r.Context() to observe the client disconnecting
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(hung.Close)

	reg := prometheus.NewRegistry()
	m := metrics.NewWithRegistry("test", reg)
	e := NewMetricsExporter("chain", ref.URL, []string{hung.URL, good.URL}, 60, zerolog.Nop()).(*exporter)
	e.rpcTimeout = time.Second

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

	// well under rpcTimeout: the good node must not wait for the hung one
	require.Eventually(t, func() bool {
		drift, ok := gaugeValue(t, reg, "test_block_height_drift", good.URL)
		return ok && drift == 10
	}, 500*time.Millisecond, 10*time.Millisecond)
	height, ok := gaugeValue(t, reg, "test_target_block_height", good.URL)
	require.True(t, ok)
	require.Equal(t, 90.0, height)

	require.Eventually(t, func() bool {
		avail, ok := gaugeValue(t, reg, "test_endpoint_availability", hung.URL)
		return ok && avail == 0
	}, 3*time.Second, 20*time.Millisecond)
}
