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

const defaultRPCTimeout = 5 * time.Second

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
		labels:         utils.RedactEndpoints(endpoints),
		scrapeInterval: scrapeInterval,
		rpcTimeout:     defaultRPCTimeout,
		logger:         logger.With().Str("component", "balance_monitor").Logger(),
	}
}

// exporter monitors account balances via consensus RPC
type exporter struct {
	chainID        string
	addresses      []string
	endpoints      []string
	labels         []string
	scrapeInterval int
	rpcTimeout     time.Duration
	logger         zerolog.Logger
}

// ExportMetrics continuously checks account balances at specified intervals
func (e *exporter) ExportMetrics(ctx context.Context, m *metrics.Metrics) error {
	ticker := time.NewTicker(time.Duration(e.scrapeInterval) * time.Second)
	defer ticker.Stop()

	// one client per endpoint, created on first use and shared across scrapes and addresses
	clients := make([]*cosmos.Client, len(e.endpoints))
	defer func() {
		for _, c := range clients {
			if c != nil {
				_ = c.Close()
			}
		}
	}()

	e.logger.Info().
		Strs("addresses", e.addresses).
		Strs("endpoints", e.labels).
		Int("scrape_interval_sec", e.scrapeInterval).
		Msg("starting balance monitoring")

	for {
		for _, address := range e.addresses {
			e.checkBalance(ctx, m, clients, address)
		}

		select {
		case <-ctx.Done():
			e.logger.Info().Msg("stopping balance monitoring")
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// checkBalance queries balance with fallback across endpoints
func (e *exporter) checkBalance(ctx context.Context, m *metrics.Metrics, clients []*cosmos.Client, address string) {
	// try each endpoint until one succeeds
	for i, endpoint := range e.endpoints {
		label := e.labels[i]
		if clients[i] == nil {
			client, err := cosmos.NewClient(endpoint, e.logger)
			if err != nil {
				e.logger.Warn().
					Err(utils.RedactError(err, endpoint)).
					Str("endpoint", label).
					Str("address", address).
					Msg("failed to create client, trying next endpoint")
				m.RecordConsensusRpcEndpointAvailability(e.chainID, label, false)
				m.RecordConsensusRpcEndpointError(e.chainID, label, utils.CategorizeError(err))
				continue
			}
			clients[i] = client
		}

		callCtx, cancel := context.WithTimeout(ctx, e.rpcTimeout)
		balances, err := clients[i].GetBalance(callCtx, address)
		cancel()

		if err != nil {
			e.logger.Warn().
				Err(utils.RedactError(err, endpoint)).
				Str("endpoint", label).
				Str("address", address).
				Msg("failed to query balance, trying next endpoint")
			m.RecordConsensusRpcEndpointAvailability(e.chainID, label, false)
			m.RecordConsensusRpcEndpointError(e.chainID, label, utils.CategorizeError(err))
			continue
		}

		// success - record availability
		m.RecordConsensusRpcEndpointAvailability(e.chainID, label, true)

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

			e.logger.Debug().
				Str("address", address).
				Str("denom", coin.Denom).
				Float64("balance", balanceFloat).
				Str("endpoint", label).
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
