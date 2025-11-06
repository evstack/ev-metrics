package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/01builders/ev-metrics/internal/clients/celestia"
	"github.com/01builders/ev-metrics/internal/clients/evm"
	"github.com/01builders/ev-metrics/internal/clients/evnode"
	"github.com/01builders/ev-metrics/pkg/exporters/drift"
	"github.com/01builders/ev-metrics/pkg/exporters/jsonrpc"
	"github.com/01builders/ev-metrics/pkg/exporters/verifier"
	"github.com/01builders/ev-metrics/pkg/metrics"
	coreda "github.com/evstack/ev-node/core/da"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
)

const (
	flagEvNodeAddr            = "evnode-addr"
	flagEvmWSURL              = "evm-ws-url"
	flagEvmRpcURL             = "evm-rpc-url"
	flagCelestiaURL           = "celestia-url"
	flagCelestiaAuthToken     = "celestia-token"
	flagHeaderNS              = "header-namespace"
	flagDataNS                = "data-namespace"
	flagDuration              = "duration"
	flagVerbose               = "verbose"
	flagPort                  = "port"
	flagChain                 = "chain-id"
	flagReferenceNode         = "reference-node"
	flagFullNodes             = "full-nodes"
	flagPollingInterval       = "polling-interval"
	flagJsonRpcScrapeInterval = "jsonrpc-scrape-interval"

	metricsPath = "/metrics"
)

var flags flagValues

type flagValues struct {
	evnodeAddr            string
	evmWSURL              string
	evmRpcURL             string
	celestiaURL           string
	celestiaAuthToken     string
	headerNS              string
	dataNS                string
	duration              int
	verbose               bool
	port                  int
	chainID               string
	referenceNode         string
	fullNodes             string
	pollingInterval       int
	jsonRpcScrapeInterval int
}

func NewMonitorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "monitor",
		Short: "Monitor EVM headers and verify corresponding DA data on Celestia",
		Long: `Subscribes to EVM block headers in real-time and for each new block:
1. Gets the blockchain height from the header
2. Queries ev-node Store API to get the DA heights where this block was published
3. Queries Celestia to verify the blobs exist at those DA heights
4. Shows the complete data flow from EVM block → ev-node → Celestia DA`,
		RunE: monitorAndExportMetrics,
	}

	cmd.Flags().StringVar(&flags.evnodeAddr, flagEvNodeAddr, "http://localhost:7331", "ev-node Connect RPC address")
	cmd.Flags().StringVar(&flags.evmWSURL, flagEvmWSURL, "ws://localhost:8546", "EVM client WebSocket URL")
	cmd.Flags().StringVar(&flags.evmRpcURL, flagEvmRpcURL, "", "EVM client JSON-RPC URL for health checks (optional, enables JSON-RPC monitoring)")
	cmd.Flags().StringVar(&flags.celestiaURL, flagCelestiaURL, "http://localhost:26658", "Celestia DA JSON-RPC URL")
	cmd.Flags().StringVar(&flags.celestiaAuthToken, flagCelestiaAuthToken, "", "Celestia authentication token (optional)")
	cmd.Flags().StringVar(&flags.headerNS, flagHeaderNS, "", "Header namespace (my_app_header_namespace)")
	cmd.Flags().StringVar(&flags.dataNS, flagDataNS, "", "Data namespace (my_app_data_namespace)")
	cmd.Flags().IntVar(&flags.duration, flagDuration, 0, "Duration in seconds to stream (0 = infinite)")
	cmd.Flags().BoolVar(&flags.verbose, flagVerbose, false, "Enable verbose logging")
	cmd.Flags().IntVar(&flags.port, flagPort, 2112, "HTTP server port for metrics (only used if --enable-metrics is set)")
	cmd.Flags().StringVar(&flags.chainID, flagChain, "testnet", "chainID identifier for metrics labels")
	cmd.Flags().StringVar(&flags.referenceNode, flagReferenceNode, "", "Reference node RPC endpoint URL (sequencer) for drift monitoring")
	cmd.Flags().StringVar(&flags.fullNodes, flagFullNodes, "", "Comma-separated list of full node RPC endpoint URLs for drift monitoring")
	cmd.Flags().IntVar(&flags.pollingInterval, flagPollingInterval, 10, "Polling interval in seconds for checking node block heights (default: 10)")
	cmd.Flags().IntVar(&flags.jsonRpcScrapeInterval, flagJsonRpcScrapeInterval, 10, "JSON-RPC health check scrape interval in seconds (default: 10)")

	if err := cmd.MarkFlagRequired(flagHeaderNS); err != nil {
		panic(err)
	}
	if err := cmd.MarkFlagRequired(flagDataNS); err != nil {
		panic(err)
	}

	return cmd
}

// initializeClientsAndLoadConfig initializes the clients and loads the configuration based on the provided flags.
func initializeClientsAndLoadConfig(ctx context.Context, logger zerolog.Logger) (*Config, error) {
	headerNS := coreda.NamespaceFromString(flags.headerNS).Bytes()
	logger.Info().Str("header_namespace", fmt.Sprintf("%x", headerNS)).Msg("using header namespace")

	dataNS := coreda.NamespaceFromString(flags.dataNS).Bytes()
	logger.Info().Str("data_namespace", fmt.Sprintf("%x", dataNS)).Msg("using data namespace")

	evNodeClient, evmClient, celestiaClient, err := newClients(ctx, flags, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create clients: %w", err)
	}

	return &Config{
		EvnodeAddr:     flags.evnodeAddr,
		EVNodeClient:   evNodeClient,
		EvmClient:      evmClient,
		CelestiaClient: celestiaClient,
		HeaderNS:       headerNS,
		DataNS:         dataNS,
	}, nil
}

type Config struct {
	EvnodeAddr     string
	EVNodeClient   *evnode.Client
	EvmClient      *evm.Client
	CelestiaClient *celestia.Client
	HeaderNS       []byte
	DataNS         []byte
}

func newClients(ctx context.Context, flags flagValues, logger zerolog.Logger) (*evnode.Client, *evm.Client, *celestia.Client, error) {
	evnodeClient := evnode.NewClient(flags.evnodeAddr, logger)

	evmClient, err := evm.NewClient(ctx, flags.evmWSURL, flags.evmRpcURL, logger)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to connect to EVM client: %w", err)
	}
	celestiaClient, err := celestia.NewClient(ctx, flags.celestiaURL, flags.celestiaAuthToken, logger)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to connect to Celestia: %w", err)
	}
	return evnodeClient, evmClient, celestiaClient, nil
}

func monitorAndExportMetrics(_ *cobra.Command, _ []string) error {
	// Setup logger
	logLevel := zerolog.InfoLevel
	if flags.verbose {
		logLevel = zerolog.DebugLevel
	}
	logger := zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339}).
		Level(logLevel).
		With().
		Timestamp().
		Logger()

	// setup context with signal handling for graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// log when signal is received
	go func() {
		<-ctx.Done()
		logger.Info().Msg("received shutdown signal (Ctrl+C), shutting down gracefully...")
	}()

	cfg, err := initializeClientsAndLoadConfig(ctx, logger)
	if err != nil {
		return fmt.Errorf("failed to initialize clients: %w", err)
	}

	defer func() {
		cfg.EvmClient.Close()
		cfg.CelestiaClient.Close()
	}()

	// Setup timeout if specified
	if flags.duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(flags.duration)*time.Second)
		defer cancel()
	}

	exporters := []metrics.Exporter{
		// by default always run the verifier exporter
		verifier.NewMetricsExporter(
			cfg.EVNodeClient,
			cfg.CelestiaClient,
			cfg.EvmClient,
			cfg.HeaderNS,
			cfg.DataNS,
			flags.chainID,
			logger,
		),
	}

	if flags.referenceNode != "" && flags.fullNodes != "" {
		fullNodeList := strings.Split(flags.fullNodes, ",")
		exporters = append(exporters, drift.NewMetricsExporter(flags.chainID, flags.referenceNode, fullNodeList, flags.pollingInterval, logger))
	}

	// Start JSON-RPC health monitoring if evm-rpc-url is provided
	if flags.evmRpcURL != "" {
		exporters = append(exporters, jsonrpc.NewMetricsExporter(flags.chainID, cfg.EvmClient, flags.jsonRpcScrapeInterval, logger))
	}

	err = metrics.StartServer(ctx, metricsPath, flags.port, logger, exporters...)

	// context.Canceled is expected during graceful shutdown, not an error
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}

	logger.Info().Msg("shutdown complete")
	return nil
}
