package balance

import (
	"context"
	"fmt"
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

	// persistent client state
	client          *cosmos.Client
	currentEndpoint string
}

// ExportMetrics continuously checks account balances at specified intervals
func (e *exporter) ExportMetrics(ctx context.Context, m *metrics.Metrics) error {
	ticker := time.NewTicker(time.Duration(e.scrapeInterval) * time.Second)
	defer ticker.Stop()

	// cleanup client on exit
	defer func() {
		if e.client != nil {
			_ = e.client.Close()
		}
	}()

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

// ensureClient ensures we have a working client, creating one if needed
func (e *exporter) ensureClient() error {
	// if we already have a client, return
	if e.client != nil {
		return nil
	}

	// try to connect to first endpoint
	if len(e.endpoints) == 0 {
		return fmt.Errorf("no endpoints configured")
	}

	endpoint := e.endpoints[0]
	client, err := cosmos.NewClient(endpoint, e.logger)
	if err != nil {
		return fmt.Errorf("failed to create client for %s: %w", endpoint, err)
	}

	e.client = client
	e.currentEndpoint = endpoint

	e.logger.Info().
		Str("endpoint", endpoint).
		Msg("established initial connection")

	return nil
}

// failoverToNextEndpoint closes current client and tries next endpoint
func (e *exporter) failoverToNextEndpoint(m *metrics.Metrics) error {
	// close current client
	if e.client != nil {
		_ = e.client.Close()
		e.client = nil
	}

	// try each endpoint
	for _, endpoint := range e.endpoints {
		// skip current endpoint
		if endpoint == e.currentEndpoint {
			continue
		}

		client, err := cosmos.NewClient(endpoint, e.logger)
		if err != nil {
			e.logger.Warn().
				Err(err).
				Str("endpoint", endpoint).
				Msg("failed to connect to endpoint during failover")
			m.RecordConsensusRpcEndpointAvailability(e.chainID, endpoint, false)
			m.RecordConsensusRpcEndpointError(e.chainID, endpoint, utils.CategorizeError(err))
			continue
		}

		e.client = client
		e.currentEndpoint = endpoint

		e.logger.Info().
			Str("endpoint", endpoint).
			Msg("failed over to new endpoint")

		return nil
	}

	return fmt.Errorf("all endpoints failed during failover")
}

// checkBalance queries balance with fallback across endpoints
func (e *exporter) checkBalance(ctx context.Context, m *metrics.Metrics, address string) {
	// ensure we have a client
	if err := e.ensureClient(); err != nil {
		e.logger.Error().
			Err(err).
			Str("address", address).
			Msg("failed to establish initial connection")
		return
	}

	// try current client
	balances, err := e.client.GetBalance(ctx, address)
	if err != nil {
		e.logger.Warn().
			Err(err).
			Str("endpoint", e.currentEndpoint).
			Str("address", address).
			Msg("failed to query balance, attempting failover")

		m.RecordConsensusRpcEndpointAvailability(e.chainID, e.currentEndpoint, false)
		m.RecordConsensusRpcEndpointError(e.chainID, e.currentEndpoint, utils.CategorizeError(err))

		// try to failover
		if failErr := e.failoverToNextEndpoint(m); failErr != nil {
			e.logger.Error().
				Err(failErr).
				Str("address", address).
				Msg("failed to failover to any endpoint")
			return
		}

		// retry with new client
		balances, err = e.client.GetBalance(ctx, address)
		if err != nil {
			e.logger.Error().
				Err(err).
				Str("endpoint", e.currentEndpoint).
				Str("address", address).
				Msg("failed to query balance after failover")
			m.RecordConsensusRpcEndpointAvailability(e.chainID, e.currentEndpoint, false)
			m.RecordConsensusRpcEndpointError(e.chainID, e.currentEndpoint, utils.CategorizeError(err))
			return
		}
	}

	// success - record availability
	m.RecordConsensusRpcEndpointAvailability(e.chainID, e.currentEndpoint, true)

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
			Str("endpoint", e.currentEndpoint).
			Msg("recorded account balance")
	}
}
