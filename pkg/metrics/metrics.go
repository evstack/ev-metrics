package metrics

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics contains Prometheus metrics for DA verification failures
type Metrics struct {
	// UnsubmittedRangeStart tracks the start of unsubmitted block ranges.
	UnsubmittedRangeStart *prometheus.GaugeVec
	// UnsubmittedRangeEnd tracks the end of unsubmitted block ranges.
	UnsubmittedRangeEnd *prometheus.GaugeVec
	// UnsubmittedBlocksTotal tracks the total number of unsubmitted blocks.
	UnsubmittedBlocksTotal *prometheus.GaugeVec
	// ReferenceBlockHeight tracks the block height for reference endpoint (sequencer).
	ReferenceBlockHeight *prometheus.GaugeVec
	// CurrentBlockHeight tracks the block height for target endpoints (operator nodes).
	CurrentBlockHeight *prometheus.GaugeVec
	// BlockHeightDrift tracks the drift between reference and target endpoints for a specific node.
	BlockHeightDrift *prometheus.GaugeVec
	// SubmissionDuration tracks DA blob submission duration percentiles over a rolling window.
	SubmissionDuration *prometheus.SummaryVec
	// SubmissionDaHeight tracks the DA height at which blocks were submitted.
	SubmissionDaHeight *prometheus.GaugeVec
	// SubmissionAttemptsTotal tracks the total number of submission attempts.
	SubmissionAttemptsTotal *prometheus.CounterVec
	// SubmissionFailuresTotal tracks the total number of failed submission attempts.
	SubmissionFailuresTotal *prometheus.CounterVec
	// LastSubmissionAttemptTime tracks the timestamp of the last submission attempt.
	LastSubmissionAttemptTime *prometheus.GaugeVec
	// LastSuccessfulSubmissionTime tracks the timestamp of the last successful submission.
	LastSuccessfulSubmissionTime *prometheus.GaugeVec
	// BlockTime tracks the time between consecutive blocks with histogram buckets for accurate SLO calculations.
	BlockTime *prometheus.HistogramVec
	// BlockTimeSummary tracks block time with percentiles over a rolling window.
	BlockTimeSummary *prometheus.SummaryVec
	// TimeSinceLastBlock tracks seconds since last block was received.
	TimeSinceLastBlock *prometheus.GaugeVec
	// BlockReceiveDelay tracks the delay between block creation and reception with histogram buckets.
	BlockReceiveDelay *prometheus.HistogramVec
	// JsonRpcRequestDuration tracks the duration of JSON-RPC requests to the EVM node.
	JsonRpcRequestDuration *prometheus.HistogramVec
	// JsonRpcRequestDurationSummary tracks JSON-RPC request duration with percentiles over a rolling window.
	JsonRpcRequestDurationSummary *prometheus.SummaryVec
	// JsonRpcRequestSloSeconds exports constant SLO thresholds for JSON-RPC requests.
	JsonRpcRequestSloSeconds *prometheus.GaugeVec
	// BlockTimeSloSeconds exports constant SLO thresholds for block time.
	BlockTimeSloSeconds *prometheus.GaugeVec
	// BlockReceiveDelaySloSeconds exports constant SLO thresholds for block receive delay.
	BlockReceiveDelaySloSeconds *prometheus.GaugeVec
	// EndpointAvailability tracks whether an endpoint is reachable (1.0 = available, 0.0 = unavailable).
	EndpointAvailability *prometheus.GaugeVec
	// EndpointErrors tracks endpoint connection errors by type.
	EndpointErrors *prometheus.CounterVec
	// AccountBalance tracks native token balance for Celestia addresses.
	AccountBalance *prometheus.GaugeVec
	// ConsensusRpcEndpointAvailability tracks consensus RPC endpoint health.
	ConsensusRpcEndpointAvailability *prometheus.GaugeVec
	// ConsensusRpcEndpointErrors tracks consensus RPC endpoint errors by type.
	ConsensusRpcEndpointErrors *prometheus.CounterVec

	// internal tracking to ensure we only record increasing DA heights
	latestHeaderDaHeight uint64
	latestDataDaHeight   uint64

	// internal tracking for block time calculation (uses arrival time for ms precision)
	lastBlockArrivalTime map[string]time.Time // key: chainID

	// lastSubmissionDurations tracks the most recent submission durations.
	lastSubmissionDurations map[string]time.Duration // key: chainID:namespace

	mu     sync.Mutex
	ranges map[string][]*blockRange // key: blobType -> sorted slice of ranges
}

type blockRange struct {
	start uint64
	end   uint64
}

// New creates a new Metrics instance using the default Prometheus registry
func New(namespace string) *Metrics {
	return NewWithRegistry(namespace, prometheus.DefaultRegisterer)
}

// NewWithRegistry creates a new Metrics instance with a custom registry
func NewWithRegistry(namespace string, registerer prometheus.Registerer) *Metrics {
	factory := promauto.With(registerer)
	m := &Metrics{
		UnsubmittedRangeStart: factory.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Name:      "unsubmitted_block_range_start",
				Help:      "start of unsubmitted block range",
			},
			[]string{"chain_id", "blob_type", "range_id"},
		),
		UnsubmittedRangeEnd: factory.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Name:      "unsubmitted_block_range_end",
				Help:      "end of unsubmitted block range",
			},
			[]string{"chain_id", "blob_type", "range_id"},
		),
		UnsubmittedBlocksTotal: factory.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Name:      "unsubmitted_blocks_total",
				Help:      "total number of unsubmitted blocks",
			},
			[]string{"chain_id", "blob_type"},
		),
		ReferenceBlockHeight: factory.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Name:      "reference_block_height",
				Help:      "current block height of the reference endpoint (sequencer)",
			},
			[]string{"chain_id", "endpoint"},
		),
		CurrentBlockHeight: factory.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Name:      "target_block_height",
				Help:      "current block height of target endpoints (operator nodes)",
			},
			[]string{"chain_id", "endpoint"},
		),
		BlockHeightDrift: factory.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Name:      "block_height_drift",
				Help:      "block height difference between reference and target endpoints (positive = target behind, negative = target ahead)",
			},
			[]string{"chain_id", "target_endpoint"},
		),
		SubmissionDuration: factory.NewSummaryVec(
			prometheus.SummaryOpts{
				Namespace: namespace,
				Name:      "submission_duration_seconds",
				Help:      "da blob submission duration from block creation to da availability",
				Objectives: map[float64]float64{
					0.5:  0.05,
					0.9:  0.01,
					0.95: 0.01,
					0.99: 0.001,
				},
				MaxAge:     60 * time.Second,
				AgeBuckets: 6,
			},
			[]string{"chain_id", "type"},
		),
		SubmissionDaHeight: factory.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Name:      "submission_da_height",
				Help:      "latest DA height for header and data submissions",
			},
			[]string{"chain_id", "type"},
		),
		BlockTime: factory.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: namespace,
				Name:      "block_time_seconds",
				Help:      "time between consecutive blocks with histogram buckets for accurate SLO calculations",
				Buckets:   []float64{0.1, 0.15, 0.2, 0.25, 0.3, 0.35, 0.4, 0.45, 0.5, 0.55, 0.6, 0.65, 0.7, 0.75, 0.8, 0.85, 0.9, 0.95, 1, 1.5, 2},
			},
			[]string{"chain_id"},
		),
		BlockTimeSummary: factory.NewSummaryVec(
			prometheus.SummaryOpts{
				Namespace: namespace,
				Name:      "block_time_summary_seconds",
				Help:      "block time with percentiles over a 60-second rolling window",
				Objectives: map[float64]float64{
					0.5:  0.05,
					0.9:  0.01,
					0.95: 0.01,
					0.99: 0.001,
				},
				MaxAge:     60 * time.Second,
				AgeBuckets: 6,
			},
			[]string{"chain_id"},
		),
		TimeSinceLastBlock: factory.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Name:      "time_since_last_block_seconds",
				Help:      "seconds since last block was received",
			},
			[]string{"chain_id"},
		),
		BlockReceiveDelay: factory.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: namespace,
				Name:      "block_receive_delay_seconds",
				Help:      "delay between block creation and reception with histogram buckets",
				Buckets:   []float64{0.1, 0.25, 0.5, 1.0, 2.0, 3.0, 5.0, 10.0, 15.0, 30.0, 60.0},
			},
			[]string{"chain_id"},
		),
		JsonRpcRequestDuration: factory.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: namespace,
				Name:      "jsonrpc_request_duration_seconds",
				Help:      "duration of JSON-RPC requests to the EVM node",
				Buckets:   []float64{0.05, 0.1, 0.15, 0.2, 0.25, 0.3, 0.35, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0},
			},
			[]string{"chain_id"},
		),
		JsonRpcRequestDurationSummary: factory.NewSummaryVec(
			prometheus.SummaryOpts{
				Namespace: namespace,
				Name:      "jsonrpc_request_duration_summary_seconds",
				Help:      "JSON-RPC request duration with percentiles over a rolling window",
				Objectives: map[float64]float64{
					0.5:  0.05,
					0.9:  0.01,
					0.95: 0.01,
					0.99: 0.001,
				},
				MaxAge:     60 * time.Second,
				AgeBuckets: 6,
			},
			[]string{"chain_id"},
		),
		JsonRpcRequestSloSeconds: factory.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Name:      "jsonrpc_request_slo_seconds",
				Help:      "SLO thresholds for JSON-RPC request duration",
			},
			[]string{"chain_id", "quantile"},
		),
		BlockTimeSloSeconds: factory.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Name:      "block_time_slo_seconds",
				Help:      "SLO thresholds for block time",
			},
			[]string{"chain_id", "quantile"},
		),
		BlockReceiveDelaySloSeconds: factory.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Name:      "block_receive_delay_slo_seconds",
				Help:      "SLO thresholds for block receive delay",
			},
			[]string{"chain_id", "quantile"},
		),
		EndpointAvailability: factory.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Name:      "endpoint_availability",
				Help:      "endpoint availability status (1.0 = available, 0.0 = unavailable)",
			},
			[]string{"chain_id", "endpoint"},
		),
		EndpointErrors: factory.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: namespace,
				Name:      "endpoint_errors_total",
				Help:      "total number of endpoint connection errors by type",
			},
			[]string{"chain_id", "endpoint", "error_type"},
		),
		AccountBalance: factory.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Name:      "account_balance",
				Help:      "native token balance for celestia addresses",
			},
			[]string{"chain_id", "address", "denom"},
		),
		ConsensusRpcEndpointAvailability: factory.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Name:      "consensus_rpc_endpoint_availability",
				Help:      "consensus rpc endpoint availability status (1.0 = available, 0.0 = unavailable)",
			},
			[]string{"chain_id", "endpoint"},
		),
		ConsensusRpcEndpointErrors: factory.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: namespace,
				Name:      "consensus_rpc_endpoint_errors_total",
				Help:      "total number of consensus rpc endpoint errors by type",
			},
			[]string{"chain_id", "endpoint", "error_type"},
		),
		SubmissionAttemptsTotal: factory.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: namespace,
				Name:      "submission_attempts_total",
				Help:      "total number of DA submission attempts",
			},
			[]string{"chain_id", "type"},
		),
		SubmissionFailuresTotal: factory.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: namespace,
				Name:      "submission_failures_total",
				Help:      "total number of failed DA submission attempts",
			},
			[]string{"chain_id", "type"},
		),
		LastSubmissionAttemptTime: factory.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Name:      "last_submission_attempt_time",
				Help:      "timestamp of the last DA submission attempt",
			},
			[]string{"chain_id", "type"},
		),
		LastSuccessfulSubmissionTime: factory.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Name:      "last_successful_submission_time",
				Help:      "timestamp of the last successful DA submission",
			},
			[]string{"chain_id", "type"},
		),
		ranges:                  make(map[string][]*blockRange),
		lastBlockArrivalTime:    make(map[string]time.Time),
		lastSubmissionDurations: make(map[string]time.Duration),
	}

	return m
}

// RecordSubmissionAttempt records a submission attempt and updates related metrics
func (m *Metrics) RecordSubmissionAttempt(chainID, submissionType string, success bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Always record the attempt
	m.SubmissionAttemptsTotal.WithLabelValues(chainID, submissionType).Inc()

	if !success {
		m.SubmissionFailuresTotal.WithLabelValues(chainID, submissionType).Inc()
	}

	// Record timestamp of this attempt
	now := time.Now()
	m.LastSubmissionAttemptTime.WithLabelValues(chainID, submissionType).Set(float64(now.Unix()))

	if success {
		m.LastSuccessfulSubmissionTime.WithLabelValues(chainID, submissionType).Set(float64(now.Unix()))
		log.Printf("DEBUG: Successful submission - chain: %s, type: %s, timestamp: %d", chainID, submissionType, now.Unix())
	} else {
		log.Printf("DEBUG: Failed submission attempt - chain: %s, type: %s, timestamp: %d", chainID, submissionType, now.Unix())
	}
}

// RecordSubmissionDaHeight records the DA height only if it's higher than previously recorded
func (m *Metrics) RecordSubmissionDaHeight(chainID, submissionType string, daHeight uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if submissionType == "header" {
		if daHeight > m.latestHeaderDaHeight {
			m.latestHeaderDaHeight = daHeight
			m.SubmissionDaHeight.WithLabelValues(chainID, "header").Set(float64(daHeight))
			// Debug log when submission DA height is recorded
			log.Printf("DEBUG: Recorded header submission DA height - chain: %s, height: %d", chainID, daHeight)
		} else {
			// Debug log when DA height is not higher than previous
			log.Printf("DEBUG: Header DA height %d not higher than previous %d for chain %s", daHeight, m.latestHeaderDaHeight, chainID)
		}
		return
	}

	if submissionType == "data" {
		if daHeight > m.latestDataDaHeight {
			m.latestDataDaHeight = daHeight
			m.SubmissionDaHeight.WithLabelValues(chainID, "data").Set(float64(daHeight))
			// Debug log when submission DA height is recorded
			log.Printf("DEBUG: Recorded data submission DA height - chain: %s, height: %d", chainID, daHeight)
		} else {
			// Debug log when DA height is not higher than previous
			log.Printf("DEBUG: Data DA height %d not higher than previous %d for chain %s", daHeight, m.latestDataDaHeight, chainID)
		}
	}
}

// RecordTotalMissingBlocks updates the total count of missing blocks metric.
func (m *Metrics) RecordTotalMissingBlocks(chainID, blobType string) {
	ranges := m.ranges[blobType]
	total := uint64(0)
	for _, r := range ranges {
		total += r.end - r.start + 1 // inclusive count
	}
	m.UnsubmittedBlocksTotal.WithLabelValues(chainID, blobType).Set(float64(total))
}

// RecordMissingBlock records a block that is missing from Celestia
func (m *Metrics) RecordMissingBlock(chainID, blobType string, blockHeight uint64) {
	m.mu.Lock()
	defer func() {
		m.RecordTotalMissingBlocks(chainID, blobType)
		m.mu.Unlock()
	}()

	ranges := m.ranges[blobType]
	if ranges == nil {
		ranges = []*blockRange{}
	}

	// find the position where this block should be inserted or merged
	idx := m.findRangeIndex(ranges, blockHeight)

	// check if block is already in a range
	if idx < len(ranges) && blockHeight >= ranges[idx].start && blockHeight <= ranges[idx].end {
		// block already tracked
		return
	}

	// check if block extends an existing range
	canMergeLeft := idx > 0 && ranges[idx-1].end+1 == blockHeight
	canMergeRight := idx < len(ranges) && ranges[idx].start-1 == blockHeight

	if canMergeLeft && canMergeRight {
		// merge two ranges
		leftRange := ranges[idx-1]
		rightRange := ranges[idx]

		m.deleteRange(chainID, blobType, leftRange)
		m.deleteRange(chainID, blobType, rightRange)

		// extend left range to include right range
		leftRange.end = rightRange.end
		m.updateRange(chainID, blobType, leftRange)

		// remove right range from slice
		m.ranges[blobType] = append(ranges[:idx], ranges[idx+1:]...)
		return
	}

	if canMergeLeft {
		// extend left range
		leftRange := ranges[idx-1]
		m.deleteRange(chainID, blobType, leftRange)
		leftRange.end = blockHeight
		m.updateRange(chainID, blobType, leftRange)
		return
	}

	if canMergeRight {
		// extend right range
		rightRange := ranges[idx]
		m.deleteRange(chainID, blobType, rightRange)
		rightRange.start = blockHeight
		m.updateRange(chainID, blobType, rightRange)
		return
	}

	// create new range
	newRange := &blockRange{
		start: blockHeight,
		end:   blockHeight,
	}
	// insert at idx
	ranges = append(ranges[:idx], append([]*blockRange{newRange}, ranges[idx:]...)...)
	m.updateRange(chainID, blobType, newRange)
	m.ranges[blobType] = ranges
}

// RemoveVerifiedBlock removes a block from the missing ranges when it gets verified
func (m *Metrics) RemoveVerifiedBlock(chainID, blobType string, blockHeight uint64) {
	m.mu.Lock()
	defer func() {
		m.RecordTotalMissingBlocks(chainID, blobType)
		m.mu.Unlock()
	}()

	ranges := m.ranges[blobType]
	if ranges == nil {
		return
	}

	// find the range containing this block
	idx := m.findRangeIndex(ranges, blockHeight)
	if idx >= len(ranges) {
		return
	}

	r := ranges[idx]

	// block not in any range, don't do anything.
	if blockHeight < r.start || blockHeight > r.end {
		return
	}

	// range contains only this block, delete it.
	if r.start == r.end {
		m.deleteRange(chainID, blobType, r)
		// remove range from slice
		m.ranges[blobType] = append(ranges[:idx], ranges[idx+1:]...)
		return
	}

	// block is at start of range, shrink the range
	if blockHeight == r.start {
		// remove from start of range
		m.deleteRange(chainID, blobType, r)
		r.start++ // modify existing range
		m.updateRange(chainID, blobType, r)
		return
	}

	// block is at end of range, shrink the range
	if blockHeight == r.end {
		// remove from end of range
		m.deleteRange(chainID, blobType, r)
		r.end-- // modify existing range
		m.updateRange(chainID, blobType, r)
		return
	}

	// block is in middle of range, split into two ranges
	oldEnd := r.end
	m.deleteRange(chainID, blobType, r)

	// update first range
	r.end = blockHeight - 1
	m.updateRange(chainID, blobType, r)

	// create new range for the second part
	newRange := &blockRange{
		start: blockHeight + 1,
		end:   oldEnd,
	}
	// insert after current range
	ranges = append(ranges[:idx+1], append([]*blockRange{newRange}, ranges[idx+1:]...)...)
	m.updateRange(chainID, blobType, newRange)

	m.ranges[blobType] = ranges
}

func (m *Metrics) updateRange(chain, blobType string, r *blockRange) {
	rangeID := m.rangeID(r)
	m.UnsubmittedRangeStart.WithLabelValues(chain, blobType, rangeID).Set(float64(r.start))
	m.UnsubmittedRangeEnd.WithLabelValues(chain, blobType, rangeID).Set(float64(r.end))
}

func (m *Metrics) deleteRange(chain, blobType string, r *blockRange) {
	rangeID := m.rangeID(r)
	m.UnsubmittedRangeStart.DeleteLabelValues(chain, blobType, rangeID)
	m.UnsubmittedRangeEnd.DeleteLabelValues(chain, blobType, rangeID)
}

// findRangeIndex finds the index of the range containing blockHeight using sort.Search
// Returns the index where blockHeight belongs (either in an existing range or insertion point)
func (m *Metrics) findRangeIndex(ranges []*blockRange, blockHeight uint64) int {
	// find the first range where start > blockHeight
	idx := sort.Search(len(ranges), func(i int) bool {
		return ranges[i].start > blockHeight
	})

	// if idx > 0, check if blockHeight is in the previous range
	if idx > 0 && blockHeight <= ranges[idx-1].end {
		return idx - 1
	}

	return idx
}

// rangeID generates the Prometheus label value for the range
func (m *Metrics) rangeID(r *blockRange) string {
	return fmt.Sprintf("%d-%d", r.start, r.end)
}

// RecordReferenceBlockHeight records the current block height of the reference endpoint
func (m *Metrics) RecordReferenceBlockHeight(chainID, endpoint string, height uint64) {
	m.ReferenceBlockHeight.WithLabelValues(chainID, endpoint).Set(float64(height))
}

// RecordCurrentBlockHeight records the current block height of a target endpoint
func (m *Metrics) RecordCurrentBlockHeight(chainID, endpoint string, height uint64) {
	m.CurrentBlockHeight.WithLabelValues(chainID, endpoint).Set(float64(height))
}

// RecordBlockHeightDrift calculates and records the drift between reference and target
func (m *Metrics) RecordBlockHeightDrift(chainID, targetEndpoint string, referenceHeight, targetHeight uint64) {
	drift := int64(referenceHeight) - int64(targetHeight)
	m.BlockHeightDrift.WithLabelValues(chainID, targetEndpoint).Set(float64(drift))
}

// RecordSubmissionDuration records the da submission duration for a given submission type
func (m *Metrics) RecordSubmissionDuration(chainID, submissionType string, duration time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.SubmissionDuration.WithLabelValues(chainID, submissionType).Observe(duration.Seconds())

	key := fmt.Sprintf("%s:%s", chainID, submissionType)
	m.lastSubmissionDurations[key] = duration

}

// RefreshSubmissionDuration re-observes the last known submission duration to keep the metric alive.
func (m *Metrics) RefreshSubmissionDuration() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for key, duration := range m.lastSubmissionDurations {
		// assuming format "chainID:namespace"
		parts := strings.Split(key, ":")
		if len(parts) == 2 {
			m.SubmissionDuration.WithLabelValues(parts[0], parts[1]).Observe(duration.Seconds())
		}
	}
}

// RecordBlockTime records the time between consecutive blocks using arrival time
// uses wall clock time when blocks arrive for millisecond precision
func (m *Metrics) RecordBlockTime(chainID string, arrivalTime time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	lastArrival, exists := m.lastBlockArrivalTime[chainID]
	if exists {
		blockTime := arrivalTime.Sub(lastArrival)
		// only record positive durations
		if blockTime > 0 {
			m.BlockTime.WithLabelValues(chainID).Observe(blockTime.Seconds())
			m.BlockTimeSummary.WithLabelValues(chainID).Observe(blockTime.Seconds())
		}
	}

	m.updateLastBlockTimeUnsafe(chainID, arrivalTime)
}

// UpdateLastBlockTime updates the last block arrival time and resets time since last block metric
// without recording block time histogram. This is used by pollers that can't measure inter-block time.
func (m *Metrics) UpdateLastBlockTime(chainID string, arrivalTime time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateLastBlockTimeUnsafe(chainID, arrivalTime)
}

// updateLastBlockTimeUnsafe is an unexported helper that updates the last block arrival time
// and resets the time since last block gauge.
// This function is not thread-safe and should be called with a lock held.
func (m *Metrics) updateLastBlockTimeUnsafe(chainID string, arrivalTime time.Time) {
	// update last seen arrival time
	m.lastBlockArrivalTime[chainID] = arrivalTime
	// reset time since last block to 0
	m.TimeSinceLastBlock.WithLabelValues(chainID).Set(0)
}

// UpdateTimeSinceLastBlock updates the time_since_last_block metric for all chains
// should be called periodically to keep the metric current.
func (m *Metrics) UpdateTimeSinceLastBlock() {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	for chainID, lastArrival := range m.lastBlockArrivalTime {
		timeSince := now.Sub(lastArrival).Seconds()
		m.TimeSinceLastBlock.WithLabelValues(chainID).Set(timeSince)
	}
}

// RecordBlockReceiveDelay records the delay between block creation and reception
func (m *Metrics) RecordBlockReceiveDelay(chainID string, delay time.Duration) {
	m.BlockReceiveDelay.WithLabelValues(chainID).Observe(delay.Seconds())
}

// RecordJsonRpcRequestDuration records the duration of a JSON-RPC request
func (m *Metrics) RecordJsonRpcRequestDuration(chainID string, duration time.Duration) {
	m.JsonRpcRequestDuration.WithLabelValues(chainID).Observe(duration.Seconds())
	m.JsonRpcRequestDurationSummary.WithLabelValues(chainID).Observe(duration.Seconds())
}

// InitializeJsonRpcSloThresholds initializes the constant SLO threshold gauges for JSON-RPC requests
func (m *Metrics) InitializeJsonRpcSloThresholds(chainID string) {
	m.JsonRpcRequestSloSeconds.WithLabelValues(chainID, "0.5").Set(0.2)
	m.JsonRpcRequestSloSeconds.WithLabelValues(chainID, "0.9").Set(0.35)
	m.JsonRpcRequestSloSeconds.WithLabelValues(chainID, "0.95").Set(0.4)
	m.JsonRpcRequestSloSeconds.WithLabelValues(chainID, "0.99").Set(0.5)
}

// InitializeBlockTimeSloThresholds initializes the constant SLO threshold gauges for block time
func (m *Metrics) InitializeBlockTimeSloThresholds(chainID string) {
	m.BlockTimeSloSeconds.WithLabelValues(chainID, "0.5").Set(2.0)
	m.BlockTimeSloSeconds.WithLabelValues(chainID, "0.9").Set(3.0)
	m.BlockTimeSloSeconds.WithLabelValues(chainID, "0.95").Set(4.0)
	m.BlockTimeSloSeconds.WithLabelValues(chainID, "0.99").Set(5.0)
}

// InitializeBlockReceiveDelaySloThresholds initializes the constant SLO threshold gauges for block receive delay
func (m *Metrics) InitializeBlockReceiveDelaySloThresholds(chainID string) {
	m.BlockReceiveDelaySloSeconds.WithLabelValues(chainID, "0.5").Set(1.0)
	m.BlockReceiveDelaySloSeconds.WithLabelValues(chainID, "0.9").Set(3.0)
	m.BlockReceiveDelaySloSeconds.WithLabelValues(chainID, "0.95").Set(5.0)
	m.BlockReceiveDelaySloSeconds.WithLabelValues(chainID, "0.99").Set(10.0)
}

// RecordEndpointAvailability records whether an endpoint is reachable
// available should be true if endpoint is reachable, false otherwise
func (m *Metrics) RecordEndpointAvailability(chainID, endpoint string, available bool) {
	value := 0.0
	if available {
		value = 1.0
	}
	m.EndpointAvailability.WithLabelValues(chainID, endpoint).Set(value)
}

// RecordEndpointError records an endpoint connection error with its type
func (m *Metrics) RecordEndpointError(chainID, endpoint, errorType string) {
	m.EndpointErrors.WithLabelValues(chainID, endpoint, errorType).Inc()
}

// RecordAccountBalance records the native token balance for a celestia address
func (m *Metrics) RecordAccountBalance(chainID, address, denom string, balance float64) {
	m.AccountBalance.WithLabelValues(chainID, address, denom).Set(balance)
}

// RecordConsensusRpcEndpointAvailability records whether a consensus rpc endpoint is reachable
func (m *Metrics) RecordConsensusRpcEndpointAvailability(chainID, endpoint string, available bool) {
	value := 0.0
	if available {
		value = 1.0
	}
	m.ConsensusRpcEndpointAvailability.WithLabelValues(chainID, endpoint).Set(value)
}

// RecordConsensusRpcEndpointError records a consensus rpc endpoint error with its type
func (m *Metrics) RecordConsensusRpcEndpointError(chainID, endpoint, errorType string) {
	m.ConsensusRpcEndpointErrors.WithLabelValues(chainID, endpoint, errorType).Inc()
}
