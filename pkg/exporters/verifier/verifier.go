package verifier

import (
	"context"
	"github.com/01builders/ev-metrics/internal/clients/celestia"
	"github.com/01builders/ev-metrics/internal/clients/evm"
	"github.com/01builders/ev-metrics/internal/clients/evnode"
	"github.com/01builders/ev-metrics/pkg/metrics"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/rs/zerolog"
	"time"
)

var _ metrics.Exporter = &exporter{}

// NewMetricsExporter creates a new exporter
func NewMetricsExporter(
	evnodeClient *evnode.Client,
	celestiaClient *celestia.Client,
	evmClient *evm.Client,
	headerNS, dataNS []byte,
	chainID string,
	logger zerolog.Logger,
) metrics.Exporter {
	return &exporter{
		evnodeClient:   evnodeClient,
		evmClient:      evmClient,
		celestiaClient: celestiaClient,
		headerNS:       headerNS,
		dataNS:         dataNS,
		chainID:        chainID,
		logger:         logger.With().Str("component", "verification_monitor").Logger(),
	}
}

// exporter handles verification of blocks against Celestia DA
type exporter struct {
	evnodeClient   *evnode.Client
	celestiaClient *celestia.Client
	evmClient      *evm.Client
	headerNS       []byte
	dataNS         []byte
	chainID        string
	logger         zerolog.Logger
}

// ExportMetrics starts the block verification monitoring loop
func (e *exporter) ExportMetrics(ctx context.Context, m *metrics.Metrics) error {
	headers := make(chan *types.Header, 10)
	sub, err := e.evmClient.SubscribeNewHead(ctx, headers)
	if err != nil {
		return err
	}
	defer sub.Unsubscribe()

	for {
		select {
		case <-ctx.Done():
			e.logger.Info().Msg("stopping block verification")
			return nil
		case header := <-headers:
			// record block arrival time for millisecond precision
			arrivalTime := time.Now()
			m.RecordBlockTime(e.chainID, arrivalTime)

			e.logger.Debug().
				Uint64("block_height", header.Number.Uint64()).
				Time("arrival_time", arrivalTime).
				Msg("received block header from subscription")

			// spawn a goroutine to handle this block's retries
			go e.verifyBlock(ctx, m, header)
		}
	}
}

func (e *exporter) onVerified(m *metrics.Metrics, namespace string, blockHeight, daHeight uint64, verified bool, submissionDuration time.Duration) {
	if verified {
		m.RecordSubmissionDaHeight(e.chainID, namespace, daHeight)
		m.RemoveVerifiedBlock(e.chainID, namespace, blockHeight)
		m.RecordSubmissionDuration(e.chainID, namespace, submissionDuration)
	} else {
		m.RecordMissingBlock(e.chainID, namespace, blockHeight)
	}
}

// verifyBlock attempts to verify a DA height for a given block status.
func (e *exporter) verifyBlock(ctx context.Context, m *metrics.Metrics, header *types.Header) {
	blockHeight := header.Number.Uint64()

	// check if block has transactions
	hasTransactions := header.TxHash != types.EmptyRootHash

	namespace := "header"
	if hasTransactions {
		namespace = "data"
	}

	blockTime := time.Unix(int64(header.Time), 0)

	logger := e.logger.With().Str("namespace", namespace).Uint64("block_height", blockHeight).Logger()
	logger.Info().
		Str("hash", header.Hash().Hex()).
		Time("time", blockTime).
		Uint64("gas_used", header.GasUsed).
		Bool("has_transactions", hasTransactions).
		Msg("processing block")

	startTime := time.Now()

	// exponential backoff intervals matching observed DA submission timing
	retryIntervals := []time.Duration{
		0, // immediate first attempt
		20 * time.Second,
		40 * time.Second,
		60 * time.Second,
		90 * time.Second,
		120 * time.Second,
	}

	for i, interval := range retryIntervals {
		retries := i + 1

		select {
		case <-ctx.Done():
			// context cancelled during graceful shutdown, not an error
			logger.Debug().Msg("block verification stopped due to shutdown")
			return
		case <-time.After(interval):
			// proceed with retry
		}

		blockResult, err := e.evnodeClient.GetBlock(ctx, blockHeight)
		if err != nil {
			logger.Warn().Err(err).Int("attempt", retries).Msg("failed to re-query block from ev-node")
			continue
		}

		daHeight := blockResult.HeaderDaHeight
		if namespace == "data" {
			daHeight = blockResult.DataDaHeight
		}

		if daHeight == 0 {
			logger.Debug().Int("attempt", retries).Msg("block still not submitted to DA, will retry")
			continue
		}

		blockResultWithBlobs, err := e.evnodeClient.GetBlockWithBlobs(ctx, blockHeight)
		if err != nil {
			logger.Warn().Err(err).Int("attempt", retries).Msg("failed to query block from ev-node")
			continue
		}

		daBlockTime, err := e.celestiaClient.GetBlockTimestamp(ctx, daHeight)
		if err != nil {
			logger.Warn().Err(err).Uint64("da_height", daHeight).Msg("failed to get da block timestamp")
			continue
		}

		// the time taken from block time to DA inclusion time.
		submissionDuration := daBlockTime.Sub(blockTime)

		switch namespace {
		case "header":
			verified, err := e.celestiaClient.VerifyBlobAtHeight(ctx, blockResultWithBlobs.HeaderBlob, daHeight, e.headerNS)

			if err != nil {
				logger.Warn().Err(err).Uint64("da_height", daHeight).Msg("verification failed")
				continue
			}

			if verified {
				logger.Info().
					Uint64("da_height", daHeight).
					Dur("duration", time.Since(startTime)).
					Msg("header blob verified on Celestia")
				e.onVerified(m, namespace, blockHeight, daHeight, true, submissionDuration)
				return
			}

			// verification failed
			if retries >= len(retryIntervals)+1 {
				logger.Error().
					Uint64("da_height", daHeight).
					Dur("duration", time.Since(startTime)).
					Msg("max retries reached - header blob not verified")
				e.onVerified(m, namespace, blockHeight, daHeight, false, 0)
				return
			}
			logger.Warn().Uint64("da_height", daHeight).Int("attempt", retries).Msg("verification failed, will retry")

		case "data":
			if len(blockResultWithBlobs.DataBlob) == 0 {
				logger.Info().
					Dur("duration", time.Since(startTime)).
					Msg("empty data block - no verification needed")
				e.onVerified(m, namespace, blockHeight, daHeight, true, submissionDuration)
				return
			}

			// perform actual verification between bytes from ev-node and Celestia.
			verified, err := e.celestiaClient.VerifyDataBlobAtHeight(ctx, blockResultWithBlobs.DataBlob, daHeight, e.dataNS)
			if err != nil {
				logger.Warn().Err(err).Uint64("da_height", daHeight).Msg("verification failed")
				continue
			}

			if verified {
				logger.Info().
					Uint64("da_height", daHeight).
					Dur("duration", time.Since(startTime)).
					Msg("data blob verified on Celestia")
				e.onVerified(m, namespace, blockHeight, daHeight, true, submissionDuration)
				return
			}

			// verification failed
			if retries >= len(retryIntervals)+1 {
				logger.Error().
					Uint64("da_height", daHeight).
					Dur("duration", time.Since(startTime)).
					Msg("max retries reached - data blob not verified")
				e.onVerified(m, namespace, blockHeight, daHeight, false, 0)
				return
			}
			logger.Warn().Uint64("da_height", daHeight).Int("attempt", retries).Msg("verification failed, will retry")

		default:
			logger.Error().Str("namespace", namespace).Msg("unknown namespace type")
			return
		}
	}

	// if loop completes without success, log final error
	logger.Error().Msg("max retries exhausted - ALERT: failed to verify block")
	e.onVerified(m, namespace, blockHeight, 0, false, 0)
}
