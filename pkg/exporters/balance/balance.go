package balance

import (
	"context"
	"math/big"
	"time"

	"github.com/evstack/ev-metrics/internal/clients/cosmos"
	"github.com/evstack/ev-metrics/pkg/metrics"
	"github.com/evstack/ev-metrics/pkg/utils"
	"github.com/rs/zerolog"
)

var _ metrics.Exporter = &exporter{}

// NewMetricsExporter creates a new balance monitoring exporter
func NewMetricsExporter(
	chainID string,
	addresses []string,
	endpoints []string,
	scrapeInterval int,
	logger zerolog.Logger,
) metrics.Exporter {
	return &exporter{
		chainID:        chainID,
		addresses:      addresses,
		endpoints:      endpoints,
		scrapeInterval: scrapeInterval,
		logger:         logger.With().Str("component", "balance_monitor").Logger(),
	}
}

// exporter monitors account balances via consensus RPC
type exporter struct {
	chainID        string
	addresses      []string
	endpoints      []string
	scrapeInterval int
	logger         zerolog.Logger
}

// ExportMetrics continuously checks account balances at specified intervals
func (e *exporter) ExportMetrics(ctx context.Context, m *metrics.Metrics) error {
	ticker := time.NewTicker(time.Duration(e.scrapeInterval) * time.Second)
	defer ticker.Stop()

	e.logger.Info().
		Strs("addresses", e.addresses).
		Strs("endpoints", e.endpoints).
		Int("scrape_interval_sec", e.scrapeInterval).
		Msg("starting balance monitoring")

	for {
		select {
		case <-ctx.Done():
			e.logger.Info().Msg("stopping balance monitoring")
			return ctx.Err()
		case <-ticker.C:
			for _, address := range e.addresses {
				e.checkBalance(ctx, m, address)
			}
		}
	}
}

// checkBalance queries balance with fallback across endpoints
func (e *exporter) checkBalance(ctx context.Context, m *metrics.Metrics, address string) {
	// try each endpoint until one succeeds
	for _, endpoint := range e.endpoints {
		// create client
		client, err := cosmos.NewClient(endpoint, e.logger)
		if err != nil {
			e.logger.Warn().
				Err(err).
				Str("endpoint", endpoint).
				Str("address", address).
				Msg("failed to create client, trying next endpoint")
			m.RecordConsensusRpcEndpointAvailability(e.chainID, endpoint, false)
			m.RecordConsensusRpcEndpointError(e.chainID, endpoint, utils.CategorizeError(err))
			continue
		}

		// query balance
		balances, err := client.GetBalance(ctx, address)

		// close client
		_ = client.Close()

		if err != nil {
			e.logger.Warn().
				Err(err).
				Str("endpoint", endpoint).
				Str("address", address).
				Msg("failed to query balance, trying next endpoint")
			m.RecordConsensusRpcEndpointAvailability(e.chainID, endpoint, false)
			m.RecordConsensusRpcEndpointError(e.chainID, endpoint, utils.CategorizeError(err))
			continue
		}

		// success - record availability
		m.RecordConsensusRpcEndpointAvailability(e.chainID, endpoint, true)

		// record balances for each denom
		for _, coin := range balances {
			// convert cosmos math.Int to string then to big.Float
			amountStr := coin.Amount.String()
			amount, ok := new(big.Float).SetString(amountStr)
			if !ok {
				e.logger.Warn().
					Str("amount", amountStr).
					Str("denom", coin.Denom).
					Msg("failed to parse balance amount")
				continue
			}

			balanceFloat, _ := amount.Float64()
			m.RecordAccountBalance(e.chainID, address, coin.Denom, balanceFloat)

			e.logger.Info().
				Str("address", address).
				Str("denom", coin.Denom).
				Float64("balance", balanceFloat).
				Str("endpoint", endpoint).
				Msg("recorded account balance")
		}

		// successfully queried from this endpoint
		return
	}

	// all endpoints failed
	e.logger.Error().
		Str("address", address).
		Msg("failed to query balance from all endpoints")
}
