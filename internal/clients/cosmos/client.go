package cosmos

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strings"

	"github.com/cosmos/cosmos-sdk/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// Client wraps a Cosmos SDK bank query client
type Client struct {
	endpoint   string
	conn       *grpc.ClientConn
	bankClient banktypes.QueryClient
	logger     zerolog.Logger
}

// parseEndpoint converts an endpoint URL into a gRPC "host:port" target and reports whether TLS
// should be used. https:// endpoints use TLS and default to port 443; http:// and bare host:port
// endpoints are plaintext. Userinfo, path, query and fragment are dropped, so credentials in the URL
// never reach the dial target, the logs or error messages.
func parseEndpoint(endpoint string) (target string, useTLS bool) {
	useTLS = strings.HasPrefix(endpoint, "https://")
	target = strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")
	if i := strings.IndexAny(target, "/?#"); i >= 0 {
		target = target[:i]
	}
	if i := strings.LastIndexByte(target, '@'); i >= 0 {
		target = target[i+1:]
	}
	if useTLS {
		if _, _, err := net.SplitHostPort(target); err != nil {
			target = net.JoinHostPort(strings.Trim(target, "[]"), "443")
		}
	}
	return target, useTLS
}

// NewClient creates a new Cosmos SDK client connected to the given endpoint
func NewClient(endpoint string, logger zerolog.Logger) (*Client, error) {
	grpcEndpoint, useTLS := parseEndpoint(endpoint)

	creds := insecure.NewCredentials()
	if useTLS {
		creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	}

	conn, err := grpc.NewClient(grpcEndpoint, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to %s: %w", grpcEndpoint, err)
	}

	logger.Debug().
		Str("endpoint", grpcEndpoint).
		Bool("tls", useTLS).
		Msg("established connection to cosmos endpoint")

	return &Client{
		endpoint:   grpcEndpoint,
		conn:       conn,
		bankClient: banktypes.NewQueryClient(conn),
		logger:     logger.With().Str("component", "cosmos_client").Logger(),
	}, nil
}

// GetBalance queries the balance of an address
func (c *Client) GetBalance(ctx context.Context, address string) (types.Coins, error) {
	resp, err := c.bankClient.AllBalances(ctx, &banktypes.QueryAllBalancesRequest{
		Address: address,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query balances: %w", err)
	}

	c.logger.Debug().
		Str("endpoint", c.endpoint).
		Str("address", address).
		Int("num_balances", len(resp.Balances)).
		Msg("successfully queried balance")

	return resp.Balances, nil
}

// Close closes the underlying gRPC connection
func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}
