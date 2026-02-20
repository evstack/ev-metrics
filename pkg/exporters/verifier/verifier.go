package verifier

import (
	"context"
	"sync"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
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
			sub.Unsubscribe()
			close(blockQueue)
			workerGroup.Wait()
			return nil
		case subErr := <-sub.Err():
			// WebSocket subscription dropped — reconnect with backoff.
			if subErr != nil {
				e.logger.Error().Err(subErr).Msg("WebSocket subscription error, reconnecting")
			} else {
				e.logger.Warn().Msg("WebSocket subscription closed, reconnecting")
			}
			sub.Unsubscribe()
			newSub := e.reconnectSubscription(ctx, headers)
			if newSub == nil {
				// context was cancelled during reconnection
				close(blockQueue)
				workerGroup.Wait()
				return nil
			}
			sub = newSub
			e.logger.Info().Msg("WebSocket subscription re-established")
		case <-refreshTicker.C:
			// ensure that submission duration is always included in the 60 second window.
			m.RefreshSubmissionDuration()
			// update time since last block metric
			m.UpdateTimeSinceLastBlock()
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
				sub.Unsubscribe()
				close(blockQueue)
				workerGroup.Wait()
				return nil
			}
		}
	}
}

// reconnectSubscription attempts to re-establish the WebSocket block header subscription
// with exponential backoff. Returns nil if the context is cancelled before reconnecting.
func (e *exporter) reconnectSubscription(ctx context.Context, headers chan *types.Header) ethereum.Subscription {
	backoff := 5 * time.Second
	const maxBackoff = 60 * time.Second

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}

		sub, err := e.evmClient.SubscribeNewHead(ctx, headers)
		if err != nil {
			e.logger.Warn().Err(err).Dur("retry_in", backoff).Msg("failed to reconnect WebSocket subscription, retrying")
			if backoff*2 < maxBackoff {
				backoff *= 2
			} else {
				backoff = maxBackoff
			}
			continue
		}
		return sub
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
		m.RecordSubmissionAttempt(e.chainID, namespace, true)
		m.RecordSubmissionDaHeight(e.chainID, namespace, daHeight)
		m.RemoveVerifiedBlock(e.chainID, namespace, blockHeight)
		m.RecordSubmissionDuration(e.chainID, namespace, submissionDuration)
	} else {
		m.RecordSubmissionAttempt(e.chainID, namespace, false)
		m.RecordMissingBlock(e.chainID, namespace, blockHeight)
	}
}

// verifyAttemptTimeout caps how long a single verification attempt (all RPC calls
// combined) may take. Without this, a slow or hung Celestia/ev-node endpoint can
// block a worker goroutine indefinitely, eventually filling the block queue and
// freezing metrics.
const verifyAttemptTimeout = 30 * time.Second

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

		if e.verifyAttempt(ctx, m, logger, retries, blockHeight, namespace, blockTime, startTime) {
			return false
		}
	}

	// if loop completes without success, log final error
	logger.Error().Msg("max retries exhausted: failed to verify block")
	e.onVerified(m, namespace, blockHeight, 0, false, 0)
	return true
}

// verifyAttempt performs one bounded RPC attempt to verify a block against Celestia DA.
// It returns true when retrying is no longer needed (verified, or permanent failure),
// and false when the caller should retry.
// Each call is bounded by verifyAttemptTimeout so workers cannot hang indefinitely
// on slow or unresponsive ev-node / Celestia endpoints.
func (e *exporter) verifyAttempt(ctx context.Context, m *metrics.Metrics, logger zerolog.Logger, retries int, blockHeight uint64, namespace string, blockTime time.Time, startTime time.Time) bool {
	attemptCtx, cancel := context.WithTimeout(ctx, verifyAttemptTimeout)
	defer cancel()

	blockResult, err := e.evnodeClient.GetBlock(attemptCtx, blockHeight)
	if err != nil {
		logger.Warn().Err(err).Int("attempt", retries).Msg("failed to re-query block from ev-node")
		return false
	}

	daHeight := blockResult.HeaderDaHeight
	if namespace == "data" {
		daHeight = blockResult.DataDaHeight
	}

	if daHeight == 0 {
		logger.Debug().Int("attempt", retries).Msg("block still not submitted to DA, will retry")
		return false
	}

	blockResultWithBlobs, err := e.evnodeClient.GetBlockWithBlobs(attemptCtx, blockHeight)
	if err != nil {
		logger.Warn().Err(err).Int("attempt", retries).Msg("failed to query block from ev-node")
		return false
	}

	daBlockTime, err := e.celestiaClient.GetBlockTimestamp(attemptCtx, daHeight)
	if err != nil {
		logger.Warn().Err(err).Uint64("da_height", daHeight).Msg("failed to get da block timestamp")
		return false
	}

	// the time taken from block time to DA inclusion time.
	submissionDuration := daBlockTime.Sub(blockTime)

	switch namespace {
	case "header":
		verified, err := e.celestiaClient.VerifyBlobAtHeight(attemptCtx, blockResultWithBlobs.HeaderBlob, daHeight, e.headerNS)
		if err != nil {
			logger.Warn().Err(err).Uint64("da_height", daHeight).Msg("verification failed")
			return false
		}
		if verified {
			logger.Info().
				Uint64("da_height", daHeight).
				Dur("duration", time.Since(startTime)).
				Msg("header blob verified on Celestia")
			e.onVerified(m, namespace, blockHeight, daHeight, true, submissionDuration)
			return true
		}

	case "data":
		if len(blockResultWithBlobs.DataBlob) == 0 {
			logger.Info().
				Dur("duration", time.Since(startTime)).
				Msg("empty data block - no verification needed")
			e.onVerified(m, namespace, blockHeight, daHeight, true, submissionDuration)
			return true
		}

		// perform actual verification between bytes from ev-node and Celestia.
		verified, err := e.celestiaClient.VerifyDataBlobAtHeight(attemptCtx, blockResultWithBlobs.DataBlob, daHeight, e.dataNS)
		if err != nil {
			logger.Warn().Err(err).Uint64("da_height", daHeight).Msg("verification failed")
			return false
		}
		if verified {
			logger.Info().
				Uint64("da_height", daHeight).
				Dur("duration", time.Since(startTime)).
				Msg("data blob verified on Celestia")
			e.onVerified(m, namespace, blockHeight, daHeight, true, submissionDuration)
			return true
		}
		logger.Warn().Uint64("da_height", daHeight).Int("attempt", retries).Msg("verification failed, will retry")

	default:
		logger.Error().Str("namespace", namespace).Msg("unknown namespace type")
		return true
	}

	return false
}
