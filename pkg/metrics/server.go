package metrics

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	"golang.org/x/sync/errgroup"
)

// Exporter defines an interface for exporting metrics.
type Exporter interface {
	ExportMetrics(ctx context.Context, m *Metrics) error
}

// StartServer starts an HTTP server to expose Prometheus metrics and a health check endpoint.
// /health is a readiness check: it fails until the header subscription is up, while it is
// down, and (when healthMaxHeaderAge > 0) when no header has arrived within that duration.
// The server shuts down gracefully when the context is cancelled.
func StartServer(ctx context.Context, metricsPath string, port int, healthMaxHeaderAge time.Duration, logger zerolog.Logger, exporters ...Exporter) error {
	// create logger with component field
	logger = logger.With().Str("component", "http_server").Logger()

	// initialize metrics (thread-safe, shared across all exporters)
	m := New("ev_metrics")

	mux := http.NewServeMux()
	mux.Handle(metricsPath, promhttp.Handler())
	mux.HandleFunc("/health", healthHandler(m, healthMaxHeaderAge))

	// derived ctx is cancelled when any exporter or the server fails, stopping everything else
	g, ctx := errgroup.WithContext(ctx)

	// start all metric exporters
	for _, exporter := range exporters {
		// capture variable for closure (required for Go < 1.22)
		exp := exporter
		g.Go(func() error {
			return exp.ExportMetrics(ctx, m)
		})
	}

	// setup HTTP server
	serverAddr := fmt.Sprintf(":%d", port)
	server := &http.Server{
		Addr:    serverAddr,
		Handler: mux,
	}

	logger.Info().
		Str("addr", serverAddr).
		Str("metrics_path", metricsPath).
		Msg("starting HTTP server for Prometheus metrics")

	// start HTTP server
	g.Go(func() error {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("metrics server failed: %w", err)
		}
		return nil
	})

	// handle graceful shutdown
	g.Go(func() error {
		<-ctx.Done()
		logger.Info().Msg("shutting down HTTP server gracefully")

		// give server 5 seconds to finish active requests
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error().Err(err).Msg("error during server shutdown")
			return fmt.Errorf("server shutdown failed: %w", err)
		}

		logger.Info().Msg("HTTP server shut down successfully")
		return nil
	})

	return g.Wait()
}

func healthHandler(m *Metrics, maxHeaderAge time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		ok, reason := m.CheckHealth(maxHeaderAge)
		if !ok {
			http.Error(w, reason, http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("OK"))
	}
}
