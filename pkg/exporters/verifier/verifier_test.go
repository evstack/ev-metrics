package verifier

import (
	"context"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/rs/zerolog"
)

func TestProcessBlocksStopsOnContextCancel(t *testing.T) {
	e := &exporter{logger: zerolog.Nop()}
	queue := make(chan *types.Header, 1)
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
