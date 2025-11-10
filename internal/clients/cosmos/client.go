package cosmos

import (
	"context"
	"fmt"
	"strings"

	"github.com/cosmos/cosmos-sdk/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Client wraps a Cosmos SDK bank query client
type Client struct {
	endpoint   string
	conn       *grpc.ClientConn
	bankClient banktypes.QueryClient
	logger     zerolog.Logger
}

// stripScheme removes http:// or https:// prefix from endpoint
func stripScheme(endpoint string) string {
	endpoint = strings.TrimPrefix(endpoint, "http://")
	endpoint = strings.TrimPrefix(endpoint, "https://")
	return endpoint
}

// NewClient creates a new Cosmos SDK client connected to the given endpoint
func NewClient(endpoint string, logger zerolog.Logger) (*Client, error) {
	// grpc expects "host:port" not "http://host:port"
	grpcEndpoint := stripScheme(endpoint)

	conn, err := grpc.NewClient(grpcEndpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to %s: %w", grpcEndpoint, err)
	}

	logger.Debug().
		Str("endpoint", grpcEndpoint).
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
