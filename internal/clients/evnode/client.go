package evnode

import (
	"context"
	"fmt"
	"net/http"

	"connectrpc.com/connect"
	evnode "github.com/evstack/ev-node/types/pb/evnode/v1"
	"github.com/evstack/ev-node/types/pb/evnode/v1/v1connect"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

type Client struct {
	storeClient v1connect.StoreServiceClient
	logger      zerolog.Logger
}

func NewClient(addr string, logger zerolog.Logger) *Client {
	httpClient := &http.Client{}
	storeClient := v1connect.NewStoreServiceClient(httpClient, addr)

	return &Client{
		storeClient: storeClient,
		logger:      logger.With().Str("component", "evnode_client").Logger(),
	}
}

// GetState queries the current state including latest block height
func (c *Client) GetState(ctx context.Context) (*evnode.GetStateResponse, error) {
	req := connect.NewRequest(&emptypb.Empty{})

	resp, err := c.storeClient.GetState(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to get state: %w", err)
	}

	return resp.Msg, nil
}

type GetBlockResult struct {
	HeaderDaHeight uint64
	DataDaHeight   uint64
	HeaderBlob     []byte
	DataBlob       []byte
}

// GetBlock retrieves block information including DA heights
func (c *Client) GetBlock(ctx context.Context, height uint64) (*evnode.GetBlockResponse, error) {
	req := connect.NewRequest(&evnode.GetBlockRequest{
		Identifier: &evnode.GetBlockRequest_Height{
			Height: height,
		},
	})

	resp, err := c.storeClient.GetBlock(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to get block %d: %w", height, err)
	}

	return resp.Msg, nil
}

// GetBlockWithBlobs retrieves block information including the marshaled blob data
func (c *Client) GetBlockWithBlobs(ctx context.Context, height uint64) (*GetBlockResult, error) {
	resp, err := c.GetBlock(ctx, height)
	if err != nil {
		return nil, err
	}

	if resp.Block == nil {
		return nil, fmt.Errorf("block %d not found", height)
	}

	// marshal header to bytes (same way ev-node does it before submitting)
	headerBlob, err := proto.Marshal(resp.Block.Header)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal header: %w", err)
	}

	// marshal data to bytes (same way ev-node does it before submitting)
	var dataBlob []byte
	if resp.Block.Data != nil {
		dataBlob, err = proto.Marshal(resp.Block.Data)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal data: %w", err)
		}
	}

	return &GetBlockResult{
		HeaderDaHeight: resp.HeaderDaHeight,
		DataDaHeight:   resp.DataDaHeight,
		HeaderBlob:     headerBlob,
		DataBlob:       dataBlob,
	}, nil
}

// GetGenesisDaHeight retrieves the DA height at which the first block was included
func (c *Client) GetGenesisDaHeight(ctx context.Context) (uint64, error) {
	req := connect.NewRequest(&emptypb.Empty{})

	resp, err := c.storeClient.GetGenesisDaHeight(ctx, req)
	if err != nil {
		return 0, fmt.Errorf("failed to get genesis DA height: %w", err)
	}

	return resp.Msg.Height, nil
}
