package metrics

import (
	"fmt"
	"sort"
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
	// SubmissionDuration tracks DA blob submission duration quantiles over a rolling window.
	SubmissionDuration *prometheus.SummaryVec
	// SubmissionDaHeight tracks the DA height at which blocks were submitted.
	SubmissionDaHeight *prometheus.GaugeVec
	// BlockTime tracks the time between consecutive blocks over a rolling window.
	BlockTime *prometheus.SummaryVec
	// JsonRpcRequestDuration tracks the duration of JSON-RPC requests to the EVM node.
	JsonRpcRequestDuration *prometheus.HistogramVec
	// JsonRpcRequestSloSeconds exports constant SLO thresholds for JSON-RPC requests.
	JsonRpcRequestSloSeconds *prometheus.GaugeVec

	// internal tracking to ensure we only record increasing DA heights
	latestHeaderDaHeight uint64
	latestDataDaHeight   uint64

	// internal tracking for block time calculation (uses arrival time for ms precision)
	lastBlockArrivalTime map[string]time.Time // key: chainID

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
		BlockTime: factory.NewSummaryVec(
			prometheus.SummaryOpts{
				Namespace: namespace,
				Name:      "block_time_seconds",
				Help:      "time between consecutive blocks over rolling window",
				Objectives: map[float64]float64{
					0.5:  0.05, // median block time
					0.9:  0.01, // p90
					0.99: 0.01, // p99
				},
				MaxAge:     5 * time.Minute,
				AgeBuckets: 5,
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
		JsonRpcRequestSloSeconds: factory.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Name:      "jsonrpc_request_slo_seconds",
				Help:      "SLO thresholds for JSON-RPC request duration",
			},
			[]string{"chain_id", "percentile"},
		),
		ranges:               make(map[string][]*blockRange),
		lastBlockArrivalTime: make(map[string]time.Time),
	}

	return m
}

// RecordSubmissionDaHeight records the DA height only if it's higher than previously recorded
func (m *Metrics) RecordSubmissionDaHeight(chainID, submissionType string, daHeight uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if submissionType == "header" {
		if daHeight > m.latestHeaderDaHeight {
			m.latestHeaderDaHeight = daHeight
			m.SubmissionDaHeight.WithLabelValues(chainID, "header").Set(float64(daHeight))
		}
		return
	}

	if submissionType == "data" {
		if daHeight > m.latestDataDaHeight {
			m.latestDataDaHeight = daHeight
			m.SubmissionDaHeight.WithLabelValues(chainID, "data").Set(float64(daHeight))
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
	m.SubmissionDuration.WithLabelValues(chainID, submissionType).Observe(duration.Seconds())
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
		}
	}

	// update last seen arrival time
	m.lastBlockArrivalTime[chainID] = arrivalTime
}

// RecordJsonRpcRequestDuration records the duration of a JSON-RPC request
func (m *Metrics) RecordJsonRpcRequestDuration(chainID string, duration time.Duration) {
	m.JsonRpcRequestDuration.WithLabelValues(chainID).Observe(duration.Seconds())
}

// InitializeJsonRpcSloThresholds initializes the constant SLO threshold gauges
func (m *Metrics) InitializeJsonRpcSloThresholds(chainID string) {
	m.JsonRpcRequestSloSeconds.WithLabelValues(chainID, "p50").Set(0.2)
	m.JsonRpcRequestSloSeconds.WithLabelValues(chainID, "p90").Set(0.35)
	m.JsonRpcRequestSloSeconds.WithLabelValues(chainID, "p95").Set(0.4)
	m.JsonRpcRequestSloSeconds.WithLabelValues(chainID, "p99").Set(0.5)
}
