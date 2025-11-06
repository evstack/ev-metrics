package drift

import (
	"context"
	"fmt"
	"github.com/01builders/ev-metrics/pkg/metrics"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/rs/zerolog"
	"time"
)

var _ metrics.Exporter = &exporter{}

// NewMetricsExporter creates and returns a metrics exporter for monitoring block height drift across multiple nodes.
func NewMetricsExporter(chainID, referenceNode string, fullNodes []string, pollingInterval int, logger zerolog.Logger) metrics.Exporter {
	return &exporter{
		chainID:         chainID,
		referenceNode:   referenceNode,
		fullNodes:       fullNodes,
		pollingInterval: pollingInterval,
		logger:          logger.With().Str("component", "drift_monitor").Logger(),
	}
}

// exporter represents an implementation of the metrics.Exporter interface for monitoring node block height data.
type exporter struct {
	chainID         string
	referenceNode   string
	fullNodes       []string
	pollingInterval int
	logger          zerolog.Logger
}

// ExportMetrics continuously checks block heights of a reference node and multiple full nodes,
// recording the heights and their drift into the provided metrics instance at specified intervals.
func (e exporter) ExportMetrics(ctx context.Context, m *metrics.Metrics) error {
	ticker := time.NewTicker(time.Duration(e.pollingInterval) * time.Second)
	defer ticker.Stop()

	e.logger.Info().
		Str("reference_node", e.referenceNode).
		Strs("full_nodes", e.fullNodes).
		Int("polling_interval_sec", e.pollingInterval).
		Msg("starting node drift monitoring")

	for {
		select {
		case <-ctx.Done():
			e.logger.Info().Msg("stopping node drift monitoring")
			return ctx.Err()
		case <-ticker.C:
			// get reference node height
			refHeight, err := getBlockHeight(ctx, e.referenceNode)
			if err != nil {
				e.logger.Error().Err(err).Str("endpoint", e.referenceNode).Msg("failed to get reference node block height")
				continue
			}

			m.RecordReferenceBlockHeight(e.chainID, e.referenceNode, refHeight)
			e.logger.Info().Uint64("height", refHeight).Str("endpoint", e.referenceNode).Msg("recorded reference node height")

			// get each full node height and calculate drift
			for _, fullNode := range e.fullNodes {
				currentHeight, err := getBlockHeight(ctx, fullNode)
				if err != nil {
					e.logger.Error().Err(err).Str("endpoint", fullNode).Msg("failed to get full node block height")
					continue
				}

				m.RecordCurrentBlockHeight(e.chainID, fullNode, currentHeight)
				m.RecordBlockHeightDrift(e.chainID, fullNode, refHeight, currentHeight)

				drift := int64(refHeight) - int64(currentHeight)
				e.logger.Info().
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
