package balance

import (
	"bytes"
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/cosmos/cosmos-sdk/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/evstack/ev-metrics/pkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type bankServer struct {
	banktypes.UnimplementedQueryServer
}

func (*bankServer) AllBalances(_ context.Context, req *banktypes.QueryAllBalancesRequest) (*banktypes.QueryAllBalancesResponse, error) {
	amount := int64(100)
	if req.Address == "addr2" {
		amount = 200
	}
	return &banktypes.QueryAllBalancesResponse{Balances: sdk.NewCoins(sdk.NewInt64Coin("utia", amount))}, nil
}

type countingListener struct {
	net.Listener
	accepted atomic.Int32
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.accepted.Add(1)
	}
	return c, err
}

func TestExportMetrics_FailoverAndClientReuse(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	lis := &countingListener{Listener: inner}
	srv := grpc.NewServer()
	banktypes.RegisterQueryServer(srv, &bankServer{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	dead, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	deadAddr := dead.Addr().String()
	require.NoError(t, dead.Close())

	reg := prometheus.NewRegistry()
	m := metrics.NewWithRegistry("test", reg)
	e := NewMetricsExporter("chain", []string{"addr1", "addr2"}, []string{deadAddr, "http://" + lis.Addr().String()}, 60, zerolog.Nop()).(*exporter)
	e.rpcTimeout = time.Second

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = e.ExportMetrics(ctx, m)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	balances := func() map[string]float64 {
		families, err := reg.Gather()
		require.NoError(t, err)
		out := map[string]float64{}
		for _, mf := range families {
			if mf.GetName() != "test_account_balance" {
				continue
			}
			for _, metric := range mf.GetMetric() {
				for _, l := range metric.GetLabel() {
					if l.GetName() == "address" {
						out[l.GetValue()] = metric.GetGauge().GetValue()
					}
				}
			}
		}
		return out
	}

	// the first scrape runs immediately rather than after the 60s interval
	require.Eventually(t, func() bool {
		return len(balances()) == 2
	}, 3*time.Second, 20*time.Millisecond)
	require.Equal(t, map[string]float64{"addr1": 100, "addr2": 200}, balances())
	require.Equal(t, int32(1), lis.accepted.Load(), "both addresses must share one connection")
}

func TestExportMetrics_EndpointSecretsNeverExposed(t *testing.T) {
	const secret = "SECRET_API_KEY"
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	banktypes.RegisterQueryServer(srv, &bankServer{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	dead, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	deadAddr := dead.Addr().String()
	require.NoError(t, dead.Close())

	var logs bytes.Buffer
	logger := zerolog.New(&logs).Level(zerolog.DebugLevel)
	reg := prometheus.NewRegistry()
	m := metrics.NewWithRegistry("test", reg)
	endpoints := []string{"http://" + deadAddr + "/v1/" + secret, "http://" + lis.Addr().String() + "/v1/" + secret}
	e := NewMetricsExporter("chain", []string{"addr1"}, endpoints, 60, logger).(*exporter)
	e.rpcTimeout = time.Second

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = e.ExportMetrics(ctx, m)
	}()

	require.Eventually(t, func() bool {
		families, err := reg.Gather()
		require.NoError(t, err)
		for _, mf := range families {
			if mf.GetName() == "test_account_balance" {
				return true
			}
		}
		return false
	}, 3*time.Second, 20*time.Millisecond)
	cancel()
	<-done

	families, err := reg.Gather()
	require.NoError(t, err)
	labels := map[string]bool{}
	for _, mf := range families {
		for _, metric := range mf.GetMetric() {
			for _, l := range metric.GetLabel() {
				require.NotContains(t, l.GetValue(), secret, "metric %s label %s", mf.GetName(), l.GetName())
				labels[l.GetValue()] = true
			}
		}
	}
	require.True(t, labels["http://"+deadAddr])
	require.NotContains(t, logs.String(), secret)
}
