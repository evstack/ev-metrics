package verifier

import (
	"context"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/evstack/ev-metrics/internal/clients/celestia"
	"github.com/evstack/ev-metrics/internal/clients/evm"
	"github.com/evstack/ev-metrics/internal/clients/evnode"
	"github.com/evstack/ev-metrics/pkg/metrics"
	"github.com/rs/zerolog"
)

var _ metrics.Exporter = &exporter{}

// NewMetricsExporter creates a new exporter
func NewMetricsExporter(
	evnodeClient *evnode.Client,
	celestiaClient *celestia.Client,
	evmClient *evm.Client,
	headerNS, dataNS []byte,
	chainID string,
	workers int,
	logger zerolog.Logger,
) metrics.Exporter {
	return &exporter{
		evnodeClient:   evnodeClient,
		evmClient:      evmClient,
		celestiaClient: celestiaClient,
		headerNS:       headerNS,
		dataNS:         dataNS,
		chainID:        chainID,
		workers:        workers,
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
	workers        int
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

	// create buffered channel for block queue
	blockQueue := make(chan *types.Header, e.workers*2)

	// start work pool
	var workerGroup sync.WaitGroup
	for i := 0; i < e.workers; i++ {
		workerGroup.Add(1)
		workerID := i
		go func() {
			defer workerGroup.Done()
			e.processBlocks(ctx, m, workerID, blockQueue)
		}()
	}

	e.logger.Info().Int("workers", e.workers).Msg("started verification work pool")

	// ticker to refresh submission duration metric every 10 seconds
	refreshTicker := time.NewTicker(10 * time.Second)
	defer refreshTicker.Stop()

	// main subscription loop
	for {
		select {
		case <-ctx.Done():
			e.logger.Info().Msg("stopping block verification")
			close(blockQueue)
			workerGroup.Wait()
			return nil
		case <-refreshTicker.C:
			// ensure that submission duration is always included in the 60 second window.
			m.RefreshSubmissionDuration()
		case header := <-headers:
			// record block arrival time for millisecond precision
			arrivalTime := time.Now()
			m.RecordBlockTime(e.chainID, arrivalTime)

			e.logger.Debug().
				Uint64("block_height", header.Number.Uint64()).
				Time("arrival_time", arrivalTime).
				Msg("received block header from subscription")

			// send block to work pool, blocking until space is available
			select {
			case blockQueue <- header:
				// block queued successfully
			case <-ctx.Done():
				close(blockQueue)
				workerGroup.Wait()
				return nil
			}
		}
	}
}

// processBlocks processes blocks from the queue
func (e *exporter) processBlocks(ctx context.Context, m *metrics.Metrics, workerID int, blockQueue chan *types.Header) {
	logger := e.logger.With().Int("worker_id", workerID).Logger()
	logger.Debug().Msg("worker started")

	for header := range blockQueue {
		reEnqueue := e.verifyBlock(ctx, m, header)
		if reEnqueue {
			// re-queue with delay in background
			go func(h *types.Header) {
				select {
				case <-time.After(5 * time.Minute):
					select {
					case blockQueue <- h:
						logger.Debug().Uint64("block", h.Number.Uint64()).Msg("re-queued block after cooldown")
					case <-ctx.Done():
						// graceful shutdown, don't send
					}
				case <-ctx.Done():
				}
			}(header)
		}
	}

	logger.Debug().Msg("worker stopped")
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
func (e *exporter) verifyBlock(ctx context.Context, m *metrics.Metrics, header *types.Header) bool {
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
			return false
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
				return false
			}

		case "data":
			if len(blockResultWithBlobs.DataBlob) == 0 {
				logger.Info().
					Dur("duration", time.Since(startTime)).
					Msg("empty data block - no verification needed")
				e.onVerified(m, namespace, blockHeight, daHeight, true, submissionDuration)
				return false
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
				return false
			}
			logger.Warn().Uint64("da_height", daHeight).Int("attempt", retries).Msg("verification failed, will retry")

		default:
			logger.Error().Str("namespace", namespace).Msg("unknown namespace type")
			return false
		}
	}

	// if loop completes without success, log final error
	logger.Error().Msg("max retries exhausted: failed to verify block")
	e.onVerified(m, namespace, blockHeight, 0, false, 0)
	return true
}
