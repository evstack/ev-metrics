package jsonrpc

import (
	"context"
	"strings"
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

		// Classify and record the error type
		errorType := classifyError(err)
		m.RecordEndpointError(chainID, evmClient.GetRPCURL(), errorType)

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

// classifyError categorizes errors into specific types for metrics
func classifyError(err error) string {
	if err == nil {
		return "none"
	}

	errStr := strings.ToLower(err.Error())

	// Check for connection refused
	if strings.Contains(errStr, "connection refused") {
		return "connection_refused"
	}

	// Check for timeout errors
	if strings.Contains(errStr, "timeout") || strings.Contains(errStr, "deadline exceeded") {
		return "timeout"
	}

	// Check for DNS/host resolution errors
	if strings.Contains(errStr, "no such host") || strings.Contains(errStr, "dns") {
		return "dns_error"
	}

	// Check for network unreachable
	if strings.Contains(errStr, "network is unreachable") || strings.Contains(errStr, "no route to host") {
		return "network_unreachable"
	}

	// Check for HTTP status errors
	if strings.Contains(errStr, "status code") {
		return "http_error"
	}

	// Check for EOF errors
	if strings.Contains(errStr, "eof") {
		return "eof"
	}

	// Check for TLS/certificate errors
	if strings.Contains(errStr, "tls") || strings.Contains(errStr, "certificate") {
		return "tls_error"
	}

	// Default to unknown error type
	return "unknown"
}
