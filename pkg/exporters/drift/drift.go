package drift

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/evstack/ev-metrics/pkg/metrics"
	"github.com/evstack/ev-metrics/pkg/utils"
	"github.com/rs/zerolog"
)

var _ metrics.Exporter = &exporter{}

const defaultRPCTimeout = 5 * time.Second

// NewMetricsExporter creates and returns a metrics exporter for monitoring block height drift across multiple nodes.
func NewMetricsExporter(chainID, referenceNode string, fullNodes []string, pollingInterval int, logger zerolog.Logger) metrics.Exporter {
	return &exporter{
		chainID:         chainID,
		referenceNode:   referenceNode,
		fullNodes:       fullNodes,
		pollingInterval: pollingInterval,
		rpcTimeout:      defaultRPCTimeout,
		logger:          logger.With().Str("component", "drift_monitor").Logger(),
	}
}

// exporter represents an implementation of the metrics.Exporter interface for monitoring node block height data.
type exporter struct {
	chainID         string
	referenceNode   string
	fullNodes       []string
	pollingInterval int
	rpcTimeout      time.Duration
	logger          zerolog.Logger
	lastHeight      uint64
}

// node holds a lazily dialed client for one endpoint. Polls never overlap, so a node is
// only ever used by one goroutine at a time.
type node struct {
	url    string
	client *ethclient.Client
}

func (n *node) close() {
	if n.client != nil {
		n.client.Close()
		n.client = nil
	}
}

// ExportMetrics continuously checks block heights of a reference node and multiple full nodes,
// recording the heights and their drift into the provided metrics instance at specified intervals.
func (e *exporter) ExportMetrics(ctx context.Context, m *metrics.Metrics) error {
	ticker := time.NewTicker(time.Duration(e.pollingInterval) * time.Second)
	defer ticker.Stop()

	ref := &node{url: e.referenceNode}
	fullNodes := make([]*node, len(e.fullNodes))
	for i, url := range e.fullNodes {
		fullNodes[i] = &node{url: url}
	}
	defer func() {
		ref.close()
		for _, n := range fullNodes {
			n.close()
		}
	}()

	e.logger.Info().
		Str("reference_node", e.referenceNode).
		Strs("full_nodes", e.fullNodes).
		Int("polling_interval_sec", e.pollingInterval).
		Msg("starting node drift monitoring")

	for {
		e.poll(ctx, m, ref, fullNodes)

		select {
		case <-ctx.Done():
			e.logger.Info().Msg("stopping node drift monitoring")
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// poll records the reference node height, then queries all full nodes concurrently and records their drift.
func (e *exporter) poll(ctx context.Context, m *metrics.Metrics, ref *node, fullNodes []*node) {
	refHeight, err := e.blockHeight(ctx, ref)
	if err != nil {
		e.logger.Error().Err(err).Str("endpoint", ref.url).Msg("failed to get reference node block height")
		m.RecordEndpointAvailability(e.chainID, ref.url, false)
		m.RecordEndpointError(e.chainID, ref.url, utils.CategorizeError(err))
		return
	}

	m.RecordEndpointAvailability(e.chainID, ref.url, true)
	m.RecordReferenceBlockHeight(e.chainID, ref.url, refHeight)
	// Also update time since last block metric when we see new blocks via polling
	// This ensures the metric stays accurate even if WebSocket subscription fails
	if refHeight > e.lastHeight {
		m.UpdateLastBlockTime(e.chainID, time.Now())
		e.lastHeight = refHeight
	}
	e.logger.Debug().Uint64("height", refHeight).Str("endpoint", ref.url).Msg("recorded reference node height")

	var wg sync.WaitGroup
	for _, n := range fullNodes {
		wg.Add(1)
		go func(n *node) {
			defer wg.Done()
			e.pollFullNode(ctx, m, n, refHeight)
		}(n)
	}
	wg.Wait()
}

func (e *exporter) pollFullNode(ctx context.Context, m *metrics.Metrics, n *node, refHeight uint64) {
	currentHeight, err := e.blockHeight(ctx, n)
	if err != nil {
		e.logger.Error().Err(err).Str("endpoint", n.url).Msg("failed to get full node block height")
		m.RecordEndpointAvailability(e.chainID, n.url, false)
		m.RecordEndpointError(e.chainID, n.url, utils.CategorizeError(err))
		return
	}

	m.RecordEndpointAvailability(e.chainID, n.url, true)
	m.RecordCurrentBlockHeight(e.chainID, n.url, currentHeight)
	m.RecordBlockHeightDrift(e.chainID, n.url, refHeight, currentHeight)

	e.logger.Debug().
		Uint64("ref_height", refHeight).
		Uint64("target_height", currentHeight).
		Int64("drift", int64(refHeight)-int64(currentHeight)).
		Str("endpoint", n.url).
		Msg("recorded full node height and drift")
}

// blockHeight queries an EVM RPC endpoint for its current block height, dialing on first use
// and dropping the client on failure so the next poll redials.
func (e *exporter) blockHeight(ctx context.Context, n *node) (uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, e.rpcTimeout)
	defer cancel()

	if n.client == nil {
		client, err := ethclient.DialContext(ctx, n.url)
		if err != nil {
			return 0, fmt.Errorf("failed to connect to %s: %w", n.url, err)
		}
		n.client = client
	}

	height, err := n.client.BlockNumber(ctx)
	if err != nil {
		n.close()
		return 0, fmt.Errorf("failed to get block number from %s: %w", n.url, err)
	}

	return height, nil
}
