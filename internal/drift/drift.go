package drift

import (
	"context"
	"fmt"
	"strings"

	"github.com/01builders/ev-metrics/internal/metrics"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/rs/zerolog"
	"time"
)

// Monitor continuously checks block heights of a reference node and multiple full nodes,
// recording the heights and their drift into the provided metrics instance at specified intervals.
func Monitor(ctx context.Context, m *metrics.Metrics, chainID string, referenceNode string, fullNodes []string, pollingInterval int, logger zerolog.Logger) error {
	ticker := time.NewTicker(time.Duration(pollingInterval) * time.Second)
	defer ticker.Stop()

	logger.Info().
		Str("reference_node", referenceNode).
		Strs("full_nodes", fullNodes).
		Int("polling_interval_sec", pollingInterval).
		Msg("starting node drift monitoring")

	for {
		select {
		case <-ctx.Done():
			logger.Info().Msg("stopping node drift monitoring")
			return ctx.Err()
		case <-ticker.C:
			// get reference node height
			refHeight, err := getBlockHeight(ctx, referenceNode)
			if err != nil {
				logger.Error().Err(err).Str("endpoint", referenceNode).Msg("failed to get reference node block height")
				// Record error for reference node
				errorType := classifyError(err)
				m.RecordEndpointError(chainID, referenceNode, errorType)
				m.RecordEndpointAvailability(chainID, referenceNode, false)
				continue
			}

			m.RecordReferenceBlockHeight(chainID, referenceNode, refHeight)
			m.RecordEndpointAvailability(chainID, referenceNode, true)
			logger.Info().Uint64("height", refHeight).Str("endpoint", referenceNode).Msg("recorded reference node height")

			// get each full node height and calculate drift
			for _, fullNode := range fullNodes {
				currentHeight, err := getBlockHeight(ctx, fullNode)
				if err != nil {
					logger.Error().Err(err).Str("endpoint", fullNode).Msg("failed to get full node block height")
					// Record error for full node
					errorType := classifyError(err)
					m.RecordEndpointError(chainID, fullNode, errorType)
					m.RecordEndpointAvailability(chainID, fullNode, false)
					continue
				}

				m.RecordCurrentBlockHeight(chainID, fullNode, currentHeight)
				m.RecordBlockHeightDrift(chainID, fullNode, refHeight, currentHeight)
				m.RecordEndpointAvailability(chainID, fullNode, true)

				drift := int64(refHeight) - int64(currentHeight)
				logger.Info().
					Uint64("ref_height", refHeight).
					Uint64("target_height", currentHeight).
					Int64("drift", drift).
					Str("endpoint", fullNode).
					Msg("recorded full node height and drift")
			}
		}
	}
}

// getBlockHeight queries an EVM RPC endpoint for its current block height
func getBlockHeight(ctx context.Context, rpcURL string) (uint64, error) {
	client, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return 0, fmt.Errorf("failed to connect to %s: %w", rpcURL, err)
	}
	defer client.Close()

	height, err := client.BlockNumber(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to get block number from %s: %w", rpcURL, err)
	}

	return height, nil
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
