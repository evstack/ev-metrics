package jsonrpc

import (
	"context"
	"time"

	"github.com/01builders/ev-metrics/internal/evm"
	"github.com/01builders/ev-metrics/internal/metrics"
	"github.com/rs/zerolog"
)

// Monitor performs periodic JSON-RPC health checks and records metrics
func Monitor(
	ctx context.Context,
	m *metrics.Metrics,
	chainID string,
	evmClient *evm.Client,
	scrapeInterval int,
	logger zerolog.Logger,
) error {
	logger = logger.With().Str("component", "jsonrpc_monitor").Logger()
	logger.Info().
		Str("chain_id", chainID).
		Int("scrape_interval_seconds", scrapeInterval).
		Msg("starting JSON-RPC health monitoring")

	ticker := time.NewTicker(time.Duration(scrapeInterval) * time.Second)
	defer ticker.Stop()

	// Perform initial health check immediately
	if err := performHealthCheck(ctx, m, chainID, evmClient, logger); err != nil {
		logger.Warn().Err(err).Msg("initial health check failed")
	}

	for {
		select {
		case <-ctx.Done():
			logger.Info().Msg("stopping JSON-RPC health monitoring")
			return ctx.Err()
		case <-ticker.C:
			if err := performHealthCheck(ctx, m, chainID, evmClient, logger); err != nil {
				logger.Warn().Err(err).Msg("health check failed")
			}
		}
	}
}

func performHealthCheck(
	ctx context.Context,
	m *metrics.Metrics,
	chainID string,
	evmClient *evm.Client,
	logger zerolog.Logger,
) error {
	duration, err := evmClient.HealthCheckRequest(ctx)
	if err != nil {
		// Record endpoint as unreachable
		m.RecordEndpointAvailability(chainID, evmClient.GetRPCURL(), false)
		return err
	}

	// Record endpoint as reachable
	m.RecordEndpointAvailability(chainID, evmClient.GetRPCURL(), true)
	m.RecordJsonRpcRequestDuration(chainID, duration)

	logger.Info().
		Dur("duration", duration).
		Float64("duration_seconds", duration.Seconds()).
		Msg("JSON-RPC health check completed")

	return nil
}
