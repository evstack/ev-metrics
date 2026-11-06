package cosmos

import (
	"context"
	"net"
	"testing"

	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

func TestParseEndpoint(t *testing.T) {
	tests := []struct {
		endpoint string
		target   string
		useTLS   bool
	}{
		{"http://h:9090", "h:9090", false},
		{"https://h", "h:443", true},
		{"https://h:443/path", "h:443", true},
		{"https://h:9443", "h:9443", true},
		{"h:9090", "h:9090", false},
		{"https://user:pass@h:9443/p", "h:9443", true},
		{"http://user:pass@h:9090", "h:9090", false},
		{"https://h?key=SECRET", "h:443", true},
	}
	for _, tt := range tests {
		t.Run(tt.endpoint, func(t *testing.T) {
			target, useTLS := parseEndpoint(tt.endpoint)
			require.Equal(t, tt.target, target)
			require.Equal(t, tt.useTLS, useTLS)
		})
	}
}

type tokenServer struct {
	banktypes.UnimplementedQueryServer
	got []string
}

func (s *tokenServer) AllBalances(ctx context.Context, _ *banktypes.QueryAllBalancesRequest) (*banktypes.QueryAllBalancesResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	s.got = md.Get("x-token")
	return &banktypes.QueryAllBalancesResponse{}, nil
}

func TestGetBalance_XToken(t *testing.T) {
	tests := []struct {
		name  string
		token string
		want  []string
	}{
		{name: "token configured", token: "secret", want: []string{"secret"}},
		{name: "no token", token: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lis, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			srv := grpc.NewServer()
			bank := &tokenServer{}
			banktypes.RegisterQueryServer(srv, bank)
			go func() { _ = srv.Serve(lis) }()
			t.Cleanup(srv.Stop)

			c, err := NewClient("http://"+lis.Addr().String(), tt.token, zerolog.Nop())
			require.NoError(t, err)
			t.Cleanup(func() { _ = c.Close() })

			_, err = c.GetBalance(context.Background(), "addr")
			require.NoError(t, err)
			require.Equal(t, tt.want, bank.got)
		})
	}
}
