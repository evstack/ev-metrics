package verifier

import (
	"context"
	"math/big"
	"sync"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/evstack/ev-metrics/internal/clients/evnode"
	"github.com/evstack/ev-metrics/pkg/metrics"
	"github.com/rs/zerolog"
)

var _ metrics.Exporter = &exporter{}

type blockSource interface {
	GetBlockWithBlobs(ctx context.Context, height uint64) (*evnode.GetBlockResult, error)
}

type daVerifier interface {
	GetBlockTimestamp(ctx context.Context, daHeight uint64) (time.Time, error)
	VerifyBlobAtHeight(ctx context.Context, blob []byte, daHeight uint64, namespace []byte) (bool, error)
	VerifyDataBlobAtHeight(ctx context.Context, unwrappedDataBlob []byte, daHeight uint64, namespace []byte) (bool, error)
}

type headSource interface {
	SubscribeNewHead(ctx context.Context, ch chan<- *types.Header) (ethereum.Subscription, error)
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
}

// job is one pending verification attempt for a block. start is zero until the
// block is first processed by a worker.
type job struct {
	header  *types.Header
	attempt int
	start   time.Time
}

// defaultRetryIntervals is the delay before each verification attempt, matching
// observed DA submission timing.
var defaultRetryIntervals = []time.Duration{
	0, // immediate first attempt
	20 * time.Second,
	40 * time.Second,
	60 * time.Second,
	90 * time.Second,
	120 * time.Second,
}

const (
	// defaultCooldown is how long to wait before re-verifying a block whose retries were exhausted.
	defaultCooldown = 5 * time.Minute
	// defaultReconnectBackoff is the initial delay before re-subscribing; it doubles up to a 60s cap.
	defaultReconnectBackoff = 5 * time.Second
	// maxBackfill caps how many heights are fetched after a gap in the header subscription.
	maxBackfill = 1000
)

// NewMetricsExporter creates a new exporter
func NewMetricsExporter(
	evnodeClient blockSource,
	celestiaClient daVerifier,
	evmClient headSource,
	headerNS, dataNS []byte,
	chainID string,
	workers int,
	logger zerolog.Logger,
) metrics.Exporter {
	return &exporter{
		evnodeClient:     evnodeClient,
		evmClient:        evmClient,
		celestiaClient:   celestiaClient,
		headerNS:         headerNS,
		dataNS:           dataNS,
		chainID:          chainID,
		workers:          workers,
		retryIntervals:   defaultRetryIntervals,
		cooldown:         defaultCooldown,
		reconnectBackoff: defaultReconnectBackoff,
		logger:           logger.With().Str("component", "verification_monitor").Logger(),
	}
}

// exporter handles verification of blocks against Celestia DA
type exporter struct {
	evnodeClient   blockSource
	celestiaClient daVerifier
	evmClient      headSource
	headerNS       []byte
	dataNS         []byte
	chainID        string
	workers        int
	retryIntervals []time.Duration
	cooldown       time.Duration
	// reconnectBackoff is the initial delay before re-subscribing after the subscription drops.
	reconnectBackoff time.Duration
	logger           zerolog.Logger
}

// ExportMetrics starts the block verification monitoring loop
func (e *exporter) ExportMetrics(ctx context.Context, m *metrics.Metrics) error {
	headers := make(chan *types.Header, 10)
	sub, err := e.evmClient.SubscribeNewHead(ctx, headers)
	if err != nil {
		return err
	}
	m.SetSubscriptionUp(true)

	// create buffered channel for block queue
	blockQueue := make(chan job, e.workers*2)

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

	// pre-initialize submission metrics so both blob types are always visible
	// in Prometheus output, even before any block has been fully processed.
	m.InitializeSubmissionMetrics(e.chainID)
	m.InitializeBlockTimeSloThresholds(e.chainID)
	m.InitializeBlockReceiveDelaySloThresholds(e.chainID)
	m.InitializeVerifierMetrics(e.chainID)

	// ticker to refresh submission duration metric every 10 seconds
	refreshTicker := time.NewTicker(10 * time.Second)
	defer refreshTicker.Stop()

	// shutdown cleanly unsubscribes and waits for all workers to finish.
	// It captures sub by reference so reassignment during reconnection is reflected.
	shutdown := func() {
		sub.Unsubscribe()
		workerGroup.Wait()
	}

	// lastHeight is the highest block seen, used to detect gaps after reconnects.
	var lastHeight uint64

	// main subscription loop
	for {
		select {
		case <-ctx.Done():
			e.logger.Info().Msg("stopping block verification")
			shutdown()
			return nil
		case subErr := <-sub.Err():
			// WebSocket subscription dropped — reconnect with backoff.
			if subErr != nil {
				e.logger.Error().Err(subErr).Msg("WebSocket subscription error, reconnecting")
			} else {
				e.logger.Warn().Msg("WebSocket subscription closed, reconnecting")
			}
			sub.Unsubscribe()
			m.SetSubscriptionUp(false)
			newSub := e.reconnectSubscription(ctx, headers)
			if newSub == nil {
				// context was cancelled during reconnection
				workerGroup.Wait()
				return nil
			}
			sub = newSub
			m.SetSubscriptionUp(true)
			m.RecordWsReconnect(e.chainID)
			e.logger.Info().Msg("WebSocket subscription re-established")
		case <-refreshTicker.C:
			// ensure that submission duration is always included in the 60 second window.
			m.RefreshSubmissionDuration()
			// update time since last block metric
			m.UpdateTimeSinceLastBlock()
			m.SetVerifierQueueLength(e.chainID, len(blockQueue))
		case header := <-headers:
			// record block arrival time for millisecond precision
			arrivalTime := time.Now()
			m.RecordBlockTime(e.chainID, arrivalTime)
			m.RecordHeaderReceived(e.chainID, arrivalTime)
			m.RecordBlockReceiveDelay(e.chainID, max(0, arrivalTime.Sub(time.Unix(int64(header.Time), 0))))

			height := header.Number.Uint64()
			e.logger.Debug().
				Uint64("block_height", height).
				Time("arrival_time", arrivalTime).
				Msg("received block header from subscription")

			if lastHeight != 0 && height > lastHeight+1 {
				workerGroup.Add(1)
				go func(from, to uint64) {
					defer workerGroup.Done()
					e.backfill(ctx, m, blockQueue, from, to)
				}(lastHeight+1, height-1)
			}
			lastHeight = max(lastHeight, height)

			// send block to work pool, blocking until space is available
			select {
			case blockQueue <- job{header: header}:
				// block queued successfully
			case <-ctx.Done():
				shutdown()
				return nil
			}
		}
	}
}

// reconnectSubscription attempts to re-establish the WebSocket block header subscription
// with exponential backoff. Returns nil if the context is cancelled before reconnecting.
func (e *exporter) reconnectSubscription(ctx context.Context, headers chan *types.Header) ethereum.Subscription {
	backoff := e.reconnectBackoff
	const maxBackoff = 60 * time.Second

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}

		sub, err := e.evmClient.SubscribeNewHead(ctx, headers)
		if err != nil {
			if backoff*2 < maxBackoff {
				backoff *= 2
			} else {
				backoff = maxBackoff
			}
			e.logger.Warn().Err(err).Dur("retry_in", backoff).Msg("failed to reconnect WebSocket subscription, retrying")
			continue
		}
		return sub
	}
}

// backfill enqueues the blocks in [from, to] that were missed by the subscription,
// keeping only the most recent maxBackfill heights.
func (e *exporter) backfill(ctx context.Context, m *metrics.Metrics, blockQueue chan<- job, from, to uint64) {
	if n := to - from + 1; n > maxBackfill {
		m.RecordBackfillDropped(e.chainID, n-maxBackfill)
		e.logger.Warn().Uint64("dropped", n-maxBackfill).Uint64("from", from).Uint64("to", to).Msg("backfill gap too large, skipping oldest blocks")
		from = to - maxBackfill + 1
	}
	e.logger.Info().Uint64("from", from).Uint64("to", to).Msg("backfilling blocks missed by subscription")

	for h := from; h <= to; h++ {
		header, err := e.evmClient.HeaderByNumber(ctx, new(big.Int).SetUint64(h))
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			e.logger.Warn().Err(err).Uint64("block_height", h).Msg("failed to fetch header for backfill, skipping")
			continue
		}
		select {
		case blockQueue <- job{header: header}:
			m.RecordBackfill(e.chainID, 1)
		case <-ctx.Done():
			return
		}
	}
}

// enqueueAfter sends j to blockQueue after d without blocking the caller.
func enqueueAfter(ctx context.Context, blockQueue chan<- job, d time.Duration, j job) {
	time.AfterFunc(d, func() {
		select {
		case blockQueue <- j:
		case <-ctx.Done():
		}
	})
}

// processBlocks processes blocks from the queue
func (e *exporter) processBlocks(ctx context.Context, m *metrics.Metrics, workerID int, blockQueue chan job) {
	logger := e.logger.With().Int("worker_id", workerID).Logger()
	logger.Debug().Msg("worker started")

	// blockQueue is never closed: delayed re-queue timers may still send to it,
	// so workers exit on ctx cancellation instead.
	for {
		select {
		case <-ctx.Done():
			logger.Debug().Msg("worker stopped")
			return
		case j := <-blockQueue:
			e.verifyBlock(ctx, m, blockQueue, j)
		}
	}
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

// verifyBlock makes one verification attempt for j and schedules the next attempt
// (or a cooldown re-queue once retries are exhausted) without blocking the worker.
func (e *exporter) verifyBlock(ctx context.Context, m *metrics.Metrics, blockQueue chan<- job, j job) {
	header := j.header
	blockHeight := header.Number.Uint64()

	// check if block has transactions
	hasTransactions := header.TxHash != types.EmptyRootHash

	namespace := "header"
	if hasTransactions {
		namespace = "data"
	}

	blockTime := time.Unix(int64(header.Time), 0)

	logger := e.logger.With().Str("namespace", namespace).Uint64("block_height", blockHeight).Logger()
	if j.start.IsZero() {
		j.start = time.Now()
		m.AddInflightBlocks(e.chainID, 1)
		logger.Debug().
			Str("hash", header.Hash().Hex()).
			Time("time", blockTime).
			Uint64("gas_used", header.GasUsed).
			Bool("has_transactions", hasTransactions).
			Msg("processing block")
	}

	if e.verifyAttempt(ctx, m, logger, j.attempt+1, blockHeight, namespace, blockTime, j.start) {
		m.AddInflightBlocks(e.chainID, -1)
		return
	}

	if next := j.attempt + 1; next < len(e.retryIntervals) {
		j.attempt = next
		enqueueAfter(ctx, blockQueue, e.retryIntervals[next], j)
		return
	}

	logger.Error().Msg("max retries exhausted: failed to verify block")
	e.onVerified(m, namespace, blockHeight, 0, false, 0)
	m.AddInflightBlocks(e.chainID, -1)
	enqueueAfter(ctx, blockQueue, e.cooldown, job{header: header})
}

// verifyAttempt performs one bounded RPC attempt to verify a block against Celestia DA.
// It returns true when retrying is no longer needed (verified, or permanent failure),
// and false when the caller should retry.
// Each call is bounded by verifyAttemptTimeout so workers cannot hang indefinitely
// on slow or unresponsive ev-node / Celestia endpoints.
func (e *exporter) verifyAttempt(ctx context.Context, m *metrics.Metrics, logger zerolog.Logger, retries int, blockHeight uint64, namespace string, blockTime time.Time, startTime time.Time) bool {
	attemptCtx, cancel := context.WithTimeout(ctx, verifyAttemptTimeout)
	defer cancel()

	blockResultWithBlobs, err := e.evnodeClient.GetBlockWithBlobs(attemptCtx, blockHeight)
	if err != nil {
		logger.Warn().Err(err).Int("attempt", retries).Msg("failed to query block from ev-node")
		return false
	}

	daHeight := blockResultWithBlobs.HeaderDaHeight
	if namespace == "data" {
		daHeight = blockResultWithBlobs.DataDaHeight
	}

	if daHeight == 0 {
		logger.Debug().Int("attempt", retries).Msg("block still not submitted to DA, will retry")
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
			logger.Debug().
				Uint64("da_height", daHeight).
				Dur("duration", time.Since(startTime)).
				Msg("header blob verified on Celestia")
			e.onVerified(m, namespace, blockHeight, daHeight, true, submissionDuration)
			return true
		}
		logger.Warn().Uint64("da_height", daHeight).Int("attempt", retries).Msg("header verification failed, will retry")

	case "data":
		if len(blockResultWithBlobs.DataBlob) == 0 {
			logger.Debug().
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
			logger.Debug().
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
