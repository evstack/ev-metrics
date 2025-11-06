package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

func TestMetrics_RecordMissingBlock(t *testing.T) {
	tests := []struct {
		name           string
		blocks         []blockToRecord
		expectedRanges []expectedRange
	}{
		{
			name: "single block creates single range",
			blocks: []blockToRecord{
				{chain: "testchain", blobType: "header", height: 100},
			},
			expectedRanges: []expectedRange{
				{blobType: "header", start: 100, end: 100},
			},
		},
		{
			name: "adjacent blocks create single range",
			blocks: []blockToRecord{
				{chain: "testchain", blobType: "header", height: 100},
				{chain: "testchain", blobType: "header", height: 101},
				{chain: "testchain", blobType: "header", height: 102},
			},
			expectedRanges: []expectedRange{
				{blobType: "header", start: 100, end: 102},
			},
		},
		{
			name: "extend range backward",
			blocks: []blockToRecord{
				{chain: "testchain", blobType: "header", height: 102},
				{chain: "testchain", blobType: "header", height: 101},
				{chain: "testchain", blobType: "header", height: 100},
			},
			expectedRanges: []expectedRange{
				{blobType: "header", start: 100, end: 102},
			},
		},
		{
			name: "multiple disjoint ranges",
			blocks: []blockToRecord{
				{chain: "testchain", blobType: "header", height: 100},
				{chain: "testchain", blobType: "header", height: 101},
				{chain: "testchain", blobType: "header", height: 102},
				{chain: "testchain", blobType: "header", height: 200},
				{chain: "testchain", blobType: "header", height: 201},
				{chain: "testchain", blobType: "header", height: 202},
			},
			expectedRanges: []expectedRange{
				{blobType: "header", start: 100, end: 102},
				{blobType: "header", start: 200, end: 202},
			},
		},
		{
			name: "different blob types create separate ranges",
			blocks: []blockToRecord{
				{chain: "testchain", blobType: "header", height: 100},
				{chain: "testchain", blobType: "data", height: 100},
			},
			expectedRanges: []expectedRange{
				{blobType: "header", start: 100, end: 100},
				{blobType: "data", start: 100, end: 100},
			},
		},
		{
			name: "extend range at both ends",
			blocks: []blockToRecord{
				{chain: "testchain", blobType: "header", height: 102},
				{chain: "testchain", blobType: "header", height: 103},
				{chain: "testchain", blobType: "header", height: 101}, // extend backward
				{chain: "testchain", blobType: "header", height: 104}, // extend forward
			},
			expectedRanges: []expectedRange{
				{blobType: "header", start: 101, end: 104},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			m := NewWithRegistry("test", reg)

			// record all blocks
			for _, block := range tt.blocks {
				m.RecordMissingBlock(block.chain, block.blobType, block.height)
			}

			// count total ranges across all blob types
			totalRanges := 0
			for _, ranges := range m.ranges {
				totalRanges += len(ranges)
			}
			require.Equal(t, len(tt.expectedRanges), totalRanges, "unexpected number of ranges")

			for _, expected := range tt.expectedRanges {
				found := false
				for blobType, ranges := range m.ranges {
					if blobType == expected.blobType {
						for _, r := range ranges {
							if r.start == expected.start && r.end == expected.end {
								found = true
								break
							}
						}
					}
					if found {
						break
					}
				}
				require.True(t, found, "expected to find range %s [%d-%d]", expected.blobType, expected.start, expected.end)
			}

			// verify total missing blocks metric for each blob type
			blobTypes := make(map[string]bool)
			for _, r := range tt.expectedRanges {
				blobTypes[r.blobType] = true
			}
			for blobType := range blobTypes {
				expectedTotal := calculateExpectedTotal(tt.expectedRanges, blobType)
				actualTotal := getMetricValue(t, reg, "test_unsubmitted_blocks_total", map[string]string{
					"chain_id":  "testchain",
					"blob_type": blobType,
				})
				require.Equal(t, float64(expectedTotal), actualTotal, "total missing blocks for %s should be %d", blobType, expectedTotal)
			}
		})
	}
}

func TestMetrics_RemoveVerifiedBlock(t *testing.T) {
	tests := []struct {
		name           string
		setupBlocks    []blockToRecord
		removeBlock    blockToRemove
		expectedRanges []expectedRange
		expectNoRanges bool
	}{
		{
			name: "remove single block range - deletes range",
			setupBlocks: []blockToRecord{
				{chain: "testchain", blobType: "header", height: 100},
			},
			removeBlock: blockToRemove{
				chain:    "testchain",
				blobType: "header",
				height:   100,
			},
			expectNoRanges: true,
		},
		{
			name: "remove from start of range",
			setupBlocks: []blockToRecord{
				{chain: "testchain", blobType: "header", height: 100},
				{chain: "testchain", blobType: "header", height: 101},
				{chain: "testchain", blobType: "header", height: 102},
				{chain: "testchain", blobType: "header", height: 103},
				{chain: "testchain", blobType: "header", height: 104},
				{chain: "testchain", blobType: "header", height: 105},
			},
			removeBlock: blockToRemove{
				chain:    "testchain",
				blobType: "header",
				height:   100,
			},
			expectedRanges: []expectedRange{
				{blobType: "header", start: 101, end: 105},
			},
		},
		{
			name: "remove from end of range",
			setupBlocks: []blockToRecord{
				{chain: "testchain", blobType: "header", height: 100},
				{chain: "testchain", blobType: "header", height: 101},
				{chain: "testchain", blobType: "header", height: 102},
				{chain: "testchain", blobType: "header", height: 103},
				{chain: "testchain", blobType: "header", height: 104},
				{chain: "testchain", blobType: "header", height: 105},
			},
			removeBlock: blockToRemove{
				chain:    "testchain",
				blobType: "header",
				height:   105,
			},
			expectedRanges: []expectedRange{
				{blobType: "header", start: 100, end: 104},
			},
		},
		{
			name: "remove from middle - splits range",
			setupBlocks: []blockToRecord{
				{chain: "testchain", blobType: "header", height: 100},
				{chain: "testchain", blobType: "header", height: 101},
				{chain: "testchain", blobType: "header", height: 102},
				{chain: "testchain", blobType: "header", height: 103},
				{chain: "testchain", blobType: "header", height: 104},
				{chain: "testchain", blobType: "header", height: 105},
			},
			removeBlock: blockToRemove{
				chain:    "testchain",
				blobType: "header",
				height:   103,
			},
			expectedRanges: []expectedRange{
				{blobType: "header", start: 100, end: 102},
				{blobType: "header", start: 104, end: 105},
			},
		},
		{
			name: "remove non-existent block - no effect",
			setupBlocks: []blockToRecord{
				{chain: "testchain", blobType: "header", height: 100},
				{chain: "testchain", blobType: "header", height: 101},
				{chain: "testchain", blobType: "header", height: 102},
				{chain: "testchain", blobType: "header", height: 103},
				{chain: "testchain", blobType: "header", height: 104},
				{chain: "testchain", blobType: "header", height: 105},
			},
			removeBlock: blockToRemove{
				chain:    "testchain",
				blobType: "header",
				height:   200,
			},
			expectedRanges: []expectedRange{
				{blobType: "header", start: 100, end: 105},
			},
		},
		{
			name: "remove from different blob type - no effect",
			setupBlocks: []blockToRecord{
				{chain: "testchain", blobType: "header", height: 100},
				{chain: "testchain", blobType: "header", height: 101},
			},
			removeBlock: blockToRemove{
				chain:    "testchain",
				blobType: "data",
				height:   100,
			},
			expectedRanges: []expectedRange{
				{blobType: "header", start: 100, end: 101},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			m := NewWithRegistry("test", reg)

			// setup: record all blocks
			for _, block := range tt.setupBlocks {
				m.RecordMissingBlock(block.chain, block.blobType, block.height)
			}

			// action: remove block
			m.RemoveVerifiedBlock(tt.removeBlock.chain, tt.removeBlock.blobType, tt.removeBlock.height)

			// verify
			if tt.expectNoRanges {
				totalRanges := 0
				for _, ranges := range m.ranges {
					totalRanges += len(ranges)
				}
				require.Equal(t, 0, totalRanges, "expected no ranges")

				// verify total blocks metric is 0
				actualTotal := getMetricValue(t, reg, "test_unsubmitted_blocks_total", map[string]string{
					"chain_id":  tt.removeBlock.chain,
					"blob_type": tt.removeBlock.blobType,
				})
				require.Equal(t, float64(0), actualTotal, "total missing blocks should be 0")
			} else {
				totalRanges := 0
				for _, ranges := range m.ranges {
					totalRanges += len(ranges)
				}
				require.Equal(t, len(tt.expectedRanges), totalRanges, "unexpected number of ranges")

				for _, expected := range tt.expectedRanges {
					found := false
					for blobType, ranges := range m.ranges {
						if blobType == expected.blobType {
							for _, r := range ranges {
								if r.start == expected.start && r.end == expected.end {
									found = true
									break
								}
							}
						}
						if found {
							break
						}
					}
					require.True(t, found, "expected to find range %s [%d-%d]", expected.blobType, expected.start, expected.end)
				}

				// verify total missing blocks metric for each blob type
				blobTypes := make(map[string]bool)
				for _, r := range tt.expectedRanges {
					blobTypes[r.blobType] = true
				}
				for blobType := range blobTypes {
					expectedTotal := calculateExpectedTotal(tt.expectedRanges, blobType)
					actualTotal := getMetricValue(t, reg, "test_unsubmitted_blocks_total", map[string]string{
						"chain_id":  tt.removeBlock.chain,
						"blob_type": blobType,
					})
					require.Equal(t, float64(expectedTotal), actualTotal, "total missing blocks for %s should be %d", blobType, expectedTotal)
				}
			}
		})
	}
}

func TestMetrics_RecordSubmissionDaHeight(t *testing.T) {

	for _, tc := range []struct {
		name           string
		daHeight       uint64
		submissionType string
		setup          func(m *Metrics)
		assertionFn    func(t *testing.T, value float64)
	}{
		{
			"header: record first submission DA height",
			100,
			"header",
			nil,
			func(t *testing.T, value float64) {
				require.Equal(t, float64(100), value, "should have recorded first submission DA height")
			},
		},
		{
			"header: can't record lower height",
			100,
			"header",
			func(m *Metrics) {
				m.RecordSubmissionDaHeight("testchain", "header", 105)
			},
			func(t *testing.T, value float64) {
				require.Equal(t, float64(105), value, "should not have overridden with lower DA height")
			},
		},
		{
			"header: jump in height",
			10000,
			"header",
			func(m *Metrics) {
				m.RecordSubmissionDaHeight("testchain", "header", 100)
			},
			func(t *testing.T, value float64) {
				require.Equal(t, float64(10000), value, "should have been able to override a higher DA height")
			},
		},
		{
			"data: record first submission DA height",
			100,
			"data",
			nil,
			func(t *testing.T, value float64) {
				require.Equal(t, float64(100), value, "should have recorded first submission DA height")
			},
		},
		{
			"data: can't record lower height",
			100,
			"data",
			func(m *Metrics) {
				m.RecordSubmissionDaHeight("testchain", "data", 105)
			},
			func(t *testing.T, value float64) {
				require.Equal(t, float64(105), value, "should not have overridden with lower DA height")
			},
		},
		{
			"data: jump in height",
			10000,
			"data",
			func(m *Metrics) {
				m.RecordSubmissionDaHeight("testchain", "data", 100)
			},
			func(t *testing.T, value float64) {
				require.Equal(t, float64(10000), value, "should have been able to override a higher DA height")
			},
		},
	} {
		reg := prometheus.NewRegistry()
		m := NewWithRegistry("test", reg)

		// perform any test setup before recording the submission height
		if tc.setup != nil {
			tc.setup(m)
		}

		m.RecordSubmissionDaHeight("testchain", tc.submissionType, tc.daHeight)

		// fetch the value of the metric for assertion.
		value := getMetricValue(t, reg, "test_submission_da_height", map[string]string{
			"chain_id":        "testchain",
			"submission_type": tc.submissionType,
		})

		t.Run(tc.name, func(t *testing.T) {
			tc.assertionFn(t, value)
		})
	}

}

func TestMetrics_ComplexScenario(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewWithRegistry("test", reg)

	// create initial range 100-110
	for i := uint64(100); i <= 110; i++ {
		m.RecordMissingBlock("testchain", "header", i)
	}

	totalRanges := 0
	for _, ranges := range m.ranges {
		totalRanges += len(ranges)
	}
	require.Equal(t, 1, totalRanges, "should start with one range")

	// verify total blocks: 100-110 = 11 blocks
	actualTotal := getMetricValue(t, reg, "test_unsubmitted_blocks_total", map[string]string{
		"chain_id":  "testchain",
		"blob_type": "header",
	})
	require.Equal(t, float64(11), actualTotal, "should have 11 total missing blocks")

	// remove block 103 - splits into two ranges
	m.RemoveVerifiedBlock("testchain", "header", 103)
	totalRanges = 0
	for _, ranges := range m.ranges {
		totalRanges += len(ranges)
	}
	require.Equal(t, 2, totalRanges, "should have two ranges after first split")

	// verify total blocks: 100-102 (3) + 104-110 (7) = 10 blocks
	actualTotal = getMetricValue(t, reg, "test_unsubmitted_blocks_total", map[string]string{
		"chain_id":  "testchain",
		"blob_type": "header",
	})
	require.Equal(t, float64(10), actualTotal, "should have 10 total missing blocks after removing block 103")

	// remove block 107 - splits second range
	m.RemoveVerifiedBlock("testchain", "header", 107)
	totalRanges = 0
	for _, ranges := range m.ranges {
		totalRanges += len(ranges)
	}
	require.Equal(t, 3, totalRanges, "should have three ranges after second split")

	// verify total blocks: 100-102 (3) + 104-106 (3) + 108-110 (3) = 9 blocks
	actualTotal = getMetricValue(t, reg, "test_unsubmitted_blocks_total", map[string]string{
		"chain_id":  "testchain",
		"blob_type": "header",
	})
	require.Equal(t, float64(9), actualTotal, "should have 9 total missing blocks after removing block 107")

	// verify final ranges: 100-102, 104-106, 108-110
	expectedRanges := []expectedRange{
		{blobType: "header", start: 100, end: 102},
		{blobType: "header", start: 104, end: 106},
		{blobType: "header", start: 108, end: 110},
	}

	for _, expected := range expectedRanges {
		found := false
		for blobType, ranges := range m.ranges {
			if blobType == expected.blobType {
				for _, r := range ranges {
					if r.start == expected.start && r.end == expected.end {
						found = true
						break
					}
				}
			}
			if found {
				break
			}
		}
		require.True(t, found, "expected to find range [%d-%d]", expected.start, expected.end)
	}

	// remove all blocks from first range
	m.RemoveVerifiedBlock("testchain", "header", 100)
	m.RemoveVerifiedBlock("testchain", "header", 101)
	m.RemoveVerifiedBlock("testchain", "header", 102)

	totalRanges = 0
	for _, ranges := range m.ranges {
		totalRanges += len(ranges)
	}
	require.Equal(t, 2, totalRanges, "should have two ranges after removing first range")

	// verify total blocks: 104-106 (3) + 108-110 (3) = 6 blocks
	actualTotal = getMetricValue(t, reg, "test_unsubmitted_blocks_total", map[string]string{
		"chain_id":  "testchain",
		"blob_type": "header",
	})
	require.Equal(t, float64(6), actualTotal, "should have 6 total missing blocks after removing first range")

	// verify remaining ranges: 104-106, 108-110
	expectedRanges = []expectedRange{
		{blobType: "header", start: 104, end: 106},
		{blobType: "header", start: 108, end: 110},
	}

	for _, expected := range expectedRanges {
		found := false
		for blobType, ranges := range m.ranges {
			if blobType == expected.blobType {
				for _, r := range ranges {
					if r.start == expected.start && r.end == expected.end {
						found = true
						break
					}
				}
			}
			if found {
				break
			}
		}
		require.True(t, found, "expected to find range [%d-%d]", expected.start, expected.end)
	}
}

// helper types for table tests
type blockToRecord struct {
	chain    string
	blobType string
	height   uint64
}

type blockToRemove struct {
	chain    string
	blobType string
	height   uint64
}

type expectedRange struct {
	blobType string
	start    uint64
	end      uint64
}

// calculateExpectedTotal calculates the total number of blocks from expected ranges
func calculateExpectedTotal(ranges []expectedRange, blobType string) uint64 {
	total := uint64(0)
	for _, r := range ranges {
		if r.blobType == blobType {
			total += r.end - r.start + 1
		}
	}
	return total
}

// getMetricValue retrieves the current value of a gauge metric
func getMetricValue(t *testing.T, reg *prometheus.Registry, metricName string, labels map[string]string) float64 {
	t.Helper()
	metrics, err := reg.Gather()
	require.NoError(t, err)

	for _, mf := range metrics {
		if mf.GetName() == metricName {
			for _, m := range mf.GetMetric() {
				// check if labels match
				match := true
				for _, label := range m.GetLabel() {
					if expectedVal, ok := labels[label.GetName()]; ok {
						if label.GetValue() != expectedVal {
							match = false
							break
						}
					}
				}
				if match && len(m.GetLabel()) == len(labels) {
					return m.GetGauge().GetValue()
				}
			}
		}
	}
	t.Fatalf("metric %s not found with labels %v", metricName, labels)
	return 0
}
