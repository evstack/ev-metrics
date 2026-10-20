package metrics

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

type failingExporter struct{ err error }

func (f failingExporter) ExportMetrics(context.Context, *Metrics) error { return f.err }

func TestStartServer_ExporterErrorStopsServer(t *testing.T) {
	l, err := net.Listen("tcp", ":0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())

	wantErr := errors.New("subscribe failed")
	done := make(chan error, 1)
	go func() {
		done <- StartServer(context.Background(), "/metrics", port, zerolog.Nop(), failingExporter{err: wantErr})
	}()

	select {
	case err := <-done:
		require.ErrorIs(t, err, wantErr)
	case <-time.After(10 * time.Second):
		t.Fatal("StartServer did not return after exporter failure")
	}
}
