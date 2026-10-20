package verifier

import (
	"context"
	"math/big"
	"slices"
	"sync"
	"testing"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/evstack/ev-metrics/internal/clients/evnode"
	"github.com/evstack/ev-metrics/pkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
)

const daOffset = 100

// fakeBlocks reports a block as not yet submitted for notYet[height] attempts
// (forever if negative), then as submitted at DA height height+daOffset.
type fakeBlocks struct {
	mu     sync.Mutex
	notYet map[uint64]int
	calls  map[uint64]int
}

func (f *fakeBlocks) GetBlockWithBlobs(_ context.Context, height uint64) (*evnode.GetBlockResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[height]++
	if n := f.notYet[height]; n < 0 || f.calls[height] <= n {
		return &evnode.GetBlockResult{}, nil
	}
	return &evnode.GetBlockResult{HeaderDaHeight: height + daOffset, HeaderBlob: []byte{1}}, nil
}

func (f *fakeBlocks) callCount(height uint64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[height]
}

type fakeDA struct {
	mu       sync.Mutex
	verified []uint64
}

func (f *fakeDA) GetBlockTimestamp(context.Context, uint64) (time.Time, error) {
	return time.Now(), nil
}

func (f *fakeDA) VerifyBlobAtHeight(_ context.Context, _ []byte, daHeight uint64, _ []byte) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verified = append(f.verified, daHeight-daOffset)
	return true, nil
}

func (f *fakeDA) VerifyDataBlobAtHeight(context.Context, []byte, uint64, []byte) (bool, error) {
	return true, nil
}

func (f *fakeDA) verifiedHeights() []uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.verified)
}

type fakeSub struct{ err chan error }

func (s fakeSub) Unsubscribe()      {}
func (s fakeSub) Err() <-chan error { return s.err }

type fakeHeads struct {
	mu      sync.Mutex
	ch      chan<- *types.Header
	fetched []uint64
}

func (f *fakeHeads) SubscribeNewHead(_ context.Context, ch chan<- *types.Header) (ethereum.Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ch = ch
	return fakeSub{err: make(chan error)}, nil
}

func (f *fakeHeads) HeaderByNumber(_ context.Context, number *big.Int) (*types.Header, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetched = append(f.fetched, number.Uint64())
	return newHeader(number.Uint64()), nil
}

func (f *fakeHeads) fetchedHeights() []uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.fetched)
}

func newHeader(height uint64) *types.Header {
	return &types.Header{Number: new(big.Int).SetUint64(height), TxHash: types.EmptyRootHash}
}

type harness struct {
	e      *exporter
	blocks *fakeBlocks
	da     *fakeDA
	heads  *fakeHeads
	m      *metrics.Metrics
	reg    *prometheus.Registry
}

func newHarness(workers int, notYet map[uint64]int, retryIntervals ...time.Duration) *harness {
	h := &harness{
		blocks: &fakeBlocks{notYet: notYet, calls: map[uint64]int{}},
		da:     &fakeDA{},
		heads:  &fakeHeads{},
		reg:    prometheus.NewRegistry(),
	}
	h.m = metrics.NewWithRegistry("test", h.reg)
	h.e = NewMetricsExporter(h.blocks, h.da, h.heads, []byte("h"), []byte("d"), "chain", workers, zerolog.Nop()).(*exporter)
	h.e.retryIntervals = retryIntervals
	h.e.cooldown = time.Hour
	return h
}

// startWorkers runs the worker pool on a fresh queue and returns the queue and
// a function that cancels the workers and waits for them to exit.
func (h *harness) startWorkers(t *testing.T) (chan job, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	queue := make(chan job, 10)
	var wg sync.WaitGroup
	for i := 0; i < h.e.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.e.processBlocks(ctx, h.m, i, queue)
		}()
	}
	stop := func() {
		cancel()
		wg.Wait()
	}
	t.Cleanup(stop)
	return queue, stop
}

func (h *harness) metric(t *testing.T, name string) float64 {
	t.Helper()
	families, err := h.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var sum float64
	for _, mf := range families {
		if mf.GetName() != "test_"+name {
			continue
		}
		for _, m := range mf.GetMetric() {
			switch {
			case m.Gauge != nil:
				sum += m.GetGauge().GetValue()
			case m.Counter != nil:
				sum += m.GetCounter().GetValue()
			case m.Histogram != nil:
				sum += float64(m.GetHistogram().GetSampleCount())
			}
		}
	}
	return sum
}

func eventually(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestVerifiedOnFirstAttempt(t *testing.T) {
	h := newHarness(1, nil, 0, time.Millisecond)
	queue, _ := h.startWorkers(t)

	queue <- job{header: newHeader(1)}

	eventually(t, "block not verified", func() bool { return h.metric(t, "submission_da_height") == 1+daOffset })
	if got := h.blocks.callCount(1); got != 1 {
		t.Fatalf("expected 1 ev-node call, got %d", got)
	}
}

func TestVerifiedAfterRetriesDoesNotBlockWorker(t *testing.T) {
	interval := 30 * time.Millisecond
	h := newHarness(1, map[uint64]int{1: 3}, 0, interval, interval, interval, interval)
	queue, _ := h.startWorkers(t)

	queue <- job{header: newHeader(1)}
	queue <- job{header: newHeader(2)}

	eventually(t, "blocks not verified", func() bool { return len(h.da.verifiedHeights()) == 2 })
	if got := h.da.verifiedHeights(); !slices.Equal(got, []uint64{2, 1}) {
		t.Fatalf("expected block 2 to be verified while block 1 waits for a retry, got order %v", got)
	}
	if got := h.blocks.callCount(1); got != 4 {
		t.Fatalf("expected 4 attempts for block 1, got %d", got)
	}
	if got := h.metric(t, "unsubmitted_blocks_total"); got != 0 {
		t.Fatalf("expected no unsubmitted blocks, got %v", got)
	}
}

func TestRetriesExhaustedRecordsMissingBlock(t *testing.T) {
	h := newHarness(1, map[uint64]int{1: -1}, 0, time.Millisecond, time.Millisecond)
	queue, stop := h.startWorkers(t)

	queue <- job{header: newHeader(1)}

	eventually(t, "missing block not recorded", func() bool { return h.metric(t, "unsubmitted_blocks_total") == 1 })
	stop()
	if got := h.blocks.callCount(1); got != 3 {
		t.Fatalf("expected 3 attempts before cooldown, got %d", got)
	}
}

func TestRetriesExhaustedRequeuesAfterCooldown(t *testing.T) {
	h := newHarness(1, map[uint64]int{1: 2}, 0, time.Millisecond)
	h.e.cooldown = 10 * time.Millisecond
	queue, _ := h.startWorkers(t)

	queue <- job{header: newHeader(1)}

	eventually(t, "missing block not cleared after cooldown", func() bool {
		return len(h.da.verifiedHeights()) == 1 && h.metric(t, "unsubmitted_blocks_total") == 0
	})
	if got := h.blocks.callCount(1); got != 3 {
		t.Fatalf("expected 3 attempts, got %d", got)
	}
}

func TestExportMetricsBackfillsGap(t *testing.T) {
	h := newHarness(2, nil, 0, time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- h.e.ExportMetrics(ctx, h.m) }()

	var headers chan<- *types.Header
	eventually(t, "not subscribed", func() bool {
		h.heads.mu.Lock()
		defer h.heads.mu.Unlock()
		headers = h.heads.ch
		return headers != nil
	})
	for _, n := range []uint64{1, 2, 6, 4} {
		headers <- newHeader(n)
		time.Sleep(time.Millisecond)
	}

	eventually(t, "backfilled blocks not verified", func() bool { return len(h.da.verifiedHeights()) == 7 })
	got := h.da.verifiedHeights()
	slices.Sort(got)
	if !slices.Equal(got, []uint64{1, 2, 3, 4, 4, 5, 6}) {
		t.Fatalf("unexpected verified heights %v", got)
	}
	if fetched := h.heads.fetchedHeights(); !slices.Equal(fetched, []uint64{3, 4, 5}) {
		t.Fatalf("expected heights 3..5 to be backfilled once, got %v", fetched)
	}
	// 4 live headers yield 3 block-time observations; backfilled headers add none.
	if got := h.metric(t, "block_time_seconds"); got != 3 {
		t.Fatalf("expected 3 block time observations, got %v", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("ExportMetrics did not return after context cancellation")
	}
}

func TestBackfillCapsGap(t *testing.T) {
	h := newHarness(1, nil)
	queue := make(chan job, maxBackfill+10)

	h.e.backfill(context.Background(), queue, 1, maxBackfill+5)

	if got := len(queue); got != maxBackfill {
		t.Fatalf("expected %d backfilled jobs, got %d", maxBackfill, got)
	}
	if first := (<-queue).header.Number.Uint64(); first != 6 {
		t.Fatalf("expected backfill to keep the most recent heights starting at 6, got %d", first)
	}
}

func TestExportMetricsStopsOnContextCancel(t *testing.T) {
	h := newHarness(2, map[uint64]int{1: -1}, 0, time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- h.e.ExportMetrics(ctx, h.m) }()

	eventually(t, "not subscribed", func() bool {
		h.heads.mu.Lock()
		defer h.heads.mu.Unlock()
		return h.heads.ch != nil
	})
	h.heads.ch <- newHeader(1)
	eventually(t, "block not attempted", func() bool { return h.blocks.callCount(1) > 0 })

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("ExportMetrics did not return after context cancellation")
	}
}

func TestProcessBlocksStopsOnContextCancel(t *testing.T) {
	e := &exporter{logger: zerolog.Nop()}
	queue := make(chan job, 1)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		e.processBlocks(ctx, nil, 0, queue)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("processBlocks did not return after context cancellation")
	}
}
