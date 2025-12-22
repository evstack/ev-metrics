# EV Metrics

Data Availability metrics and monitoring tool for evstack using Celestia DA. This tool monitors EVM block headers in real-time, queries ev-node for DA submission information, and verifies blob data on Celestia.

## Features

- Real-time EVM block header streaming
- Automatic verification of header and data blobs on Celestia
- Retry logic with exponential backoff for pending DA submissions
- Prometheus metrics for tracking unverified block ranges
- Support for both streaming and one-shot block verification modes
- Account balance monitoring via Celestia consensus RPC with automatic failover

## Quick Start

### Installation

```bash
# Clone the repository
git clone https://github.com/evstack/ev-metrics.git
cd ev-metrics

# Install dependencies
go mod download

# Build the binary
go build -o ev-metrics
```

### Running the Monitor

The monitor command streams EVM block headers and verifies DA submission on Celestia:

```bash
./ev-metrics monitor \
  --header-namespace testnet_header \
  --data-namespace testnet_data
```


### Enable Prometheus Metrics

```bash
./ev-metrics monitor \
  --header-namespace collect_testnet_header \
  --data-namespace collect_testnet_data \
  --enable-metrics \
  --port 2112
```

Metrics will be available at `http://localhost:2112/metrics`

## Configuration

### Command-Line Flags

**Required:**
- `--header-namespace`: Header namespace (e.g. testnet_header )
- `--data-namespace`: Data namespace (e.g. testnet_data )

**Optional:**
- `--evnode-addr`: ev-node Connect RPC address (default: `http://localhost:7331`)
- `--evm-ws-url`: EVM client WebSocket URL (default: `ws://localhost:8546`)
- `--evm-rpc-url`: EVM client JSON-RPC URL for health checks (optional, enables JSON-RPC monitoring)
- `--celestia-url`: Celestia DA JSON-RPC URL (default: `http://localhost:26658`)
- `--celestia-token`: Celestia authentication token (optional)
- `--duration`: Duration in seconds to stream (0 = infinite)
- `--chain-id`: Chain identifier for metrics labels (default: "testnet")
- `--enable-metrics`: Enable Prometheus metrics HTTP server (default: false)
- `--port`: HTTP server port for metrics (default: 2112)
- `--jsonrpc-scrape-interval`: JSON-RPC health check scrape interval in seconds (default: 10)
- `--reference-node`: Reference node RPC endpoint URL (sequencer) for drift monitoring
- `--full-nodes`: Comma-separated list of full node RPC endpoint URLs for drift monitoring
- `--polling-interval`: Polling interval in seconds for checking node block heights (default: 10)
- `--balance.addresses`: Comma-separated Celestia addresses to monitor (enables balance checking)
- `--balance.consensus-rpc-urls`: Comma-separated consensus RPC URLs for balance queries (required if balance.addresses is set)
- `--balance.scrape-interval`: Balance check scrape interval in seconds (default: 30)
- `--verifier.workers`: Number of concurrent workers for block verification (default: 50)
- `--verbose`: Enable verbose logging (default: false)
- `--debug`: Enable debug logging for submission details (default: false, can also be set via `EVMETRICS_DEBUG=true` environment variable)

### Example with Custom Endpoints

```bash
./ev-metrics monitor \
  --header-namespace collect_testnet_header \
  --data-namespace collect_testnet_data \
  --evnode-addr "http://my-evnode:7331" \
  --evm-ws-url "ws://my-evnode:8546" \
  --celestia-url "http://my-celestia:26658"
```

### Example with JSON-RPC Health Monitoring

Enable JSON-RPC request duration monitoring by providing the `--evm-rpc-url` flag:

```bash
./ev-metrics monitor \
  --header-namespace collect_testnet_header \
  --data-namespace collect_testnet_data \
  --evm-ws-url "ws://localhost:8546" \
  --evm-rpc-url "http://localhost:8545" \
  --enable-metrics \
  --jsonrpc-scrape-interval 10
```

This will periodically send `eth_blockNumber` JSON-RPC requests to monitor node health and response times.

### Example with Balance Monitoring

Monitor native token balances for one or more Celestia addresses:

```bash
./ev-metrics monitor \
  --header-namespace collect_testnet_header \
  --data-namespace collect_testnet_data \
  --balance.addresses "celestia1abc...,celestia1def..." \
  --balance.consensus-rpc-urls "https://rpc.celestia.org,https://rpc-mocha.pops.one" \
  --balance.scrape-interval 30 \
  --enable-metrics
```

This will query account balances every 30 seconds with automatic failover between RPC endpoints.

## Prometheus Metrics

When metrics are enabled, the following metrics are exposed:

### `ev_metrics_unsubmitted_block_range_start`
- **Type**: Gauge
- **Labels**: `chain_id`, `blob_type`, `range_id`
- **Description**: Start block height of unverified block ranges

### `ev_metrics_unsubmitted_block_range_end`
- **Type**: Gauge
- **Labels**: `chain_id`, `blob_type`, `range_id`
- **Description**: End block height of unverified block ranges

### `ev_metrics_unsubmitted_blocks_total`
- **Type**: Gauge
- **Labels**: `chain_id`, `blob_type`
- **Description**: Total number of unsubmitted blocks

### `ev_metrics_submission_duration_seconds`
- **Type**: Summary
- **Labels**: `chain_id`, `type`
- **Description**: DA blob submission duration from block creation to DA availability

### `ev_metrics_submission_da_height`
- **Type**: Gauge
- **Labels**: `chain_id`, `type`
- **Description**: Latest DA height for header and data submissions

### `ev_metrics_submission_attempts_total`
- **Type**: Counter
- **Labels**: `chain_id`, `type`
- **Description**: Total number of DA submission attempts (both successful and failed)

### `ev_metrics_submission_failures_total`
- **Type**: Counter
- **Labels**: `chain_id`, `type`
- **Description**: Total number of failed DA submission attempts

### `ev_metrics_last_submission_attempt_time`
- **Type**: Gauge
- **Labels**: `chain_id`, `type`
- **Description**: Timestamp of the last DA submission attempt (Unix timestamp)

### `ev_metrics_last_successful_submission_time`
- **Type**: Gauge
- **Labels**: `chain_id`, `type`
- **Description**: Timestamp of the last successful DA submission (Unix timestamp)

### Block Time Metrics

### `ev_metrics_block_time_seconds`
- **Type**: Histogram
- **Labels**: `chain_id`
- **Description**: Time between consecutive blocks with histogram buckets for accurate SLO calculations
- **Buckets**: 0.1, 0.15, 0.2, 0.25, 0.3, 0.35, 0.4, 0.45, 0.5, 0.55, 0.6, 0.65, 0.7, 0.75, 0.8, 0.85, 0.9, 0.95, 1, 1.5, 2 seconds

### `ev_metrics_block_time_summary_seconds`
- **Type**: Summary
- **Labels**: `chain_id`
- **Description**: Block time with percentiles over a 60-second rolling window
- **Note**: Will show NaN when no blocks have been received in the last 60 seconds

### `ev_metrics_time_since_last_block_seconds`
- **Type**: Gauge
- **Labels**: `chain_id`
- **Description**: Seconds since last block was received. Use this metric for alerting on stale blocks.
- **Alerting**: Alert when this value exceeds 60 seconds to detect block production issues before summary metrics show NaN

### `ev_metrics_block_time_slo_seconds`
- **Type**: Gauge
- **Labels**: `chain_id`, `quantile`
- **Description**: SLO thresholds for block time
- **Values**:
  - `0.5`: 2.0s
  - `0.9`: 3.0s
  - `0.95`: 4.0s
  - `0.99`: 5.0s

### `ev_metrics_block_receive_delay_seconds`
- **Type**: Histogram
- **Labels**: `chain_id`
- **Description**: Delay between block creation and reception with histogram buckets
- **Buckets**: 0.1, 0.25, 0.5, 1.0, 2.0, 3.0, 5.0, 10.0, 15.0, 30.0, 60.0 seconds

### `ev_metrics_block_receive_delay_slo_seconds`
- **Type**: Gauge
- **Labels**: `chain_id`, `quantile`
- **Description**: SLO thresholds for block receive delay
- **Values**:
  - `0.5`: 1.0s
  - `0.9`: 3.0s
  - `0.95`: 5.0s
  - `0.99`: 10.0s

### JSON-RPC Monitoring Metrics

When `--evm-rpc-url` is provided:

### `ev_metrics_jsonrpc_request_duration_seconds`
- **Type**: Histogram
- **Labels**: `chain_id`
- **Description**: Duration of JSON-RPC requests to the EVM node (enabled when `--evm-rpc-url` is provided)
- **Buckets**: 0.05, 0.1, 0.15, 0.2, 0.25, 0.3, 0.35, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0 seconds

### `ev_metrics_jsonrpc_request_slo_seconds`
- **Type**: Gauge
- **Labels**: `chain_id`, `quantile`
- **Description**: SLO thresholds for JSON-RPC request duration (enabled when `--evm-rpc-url` is provided)
- **Values**:
  - `0.5`: 0.2s
  - `0.9`: 0.35s
  - `0.95`: 0.4s
  - `0.99`: 0.5s

### Block Height Drift Metrics

When `--reference-node` and `--full-nodes` are provided:

### `ev_metrics_reference_block_height`
- **Type**: Gauge
- **Labels**: `chain_id`, `endpoint`
- **Description**: Current block height of the reference endpoint (sequencer)

### `ev_metrics_target_block_height`
- **Type**: Gauge
- **Labels**: `chain_id`, `endpoint`
- **Description**: Current block height of target endpoints (operator nodes)

### `ev_metrics_block_height_drift`
- **Type**: Gauge
- **Labels**: `chain_id`, `target_endpoint`
- **Description**: Block height difference between reference and target endpoints (positive = target behind, negative = target ahead)

### Balance Monitoring Metrics

When `--balance.addresses` and `--balance.consensus-rpc-urls` are provided:

### `ev_metrics_account_balance`
- **Type**: Gauge
- **Labels**: `chain_id`, `address`, `denom`
- **Description**: Native token balance for Celestia addresses

### `ev_metrics_consensus_rpc_endpoint_availability`
- **Type**: Gauge
- **Labels**: `chain_id`, `endpoint`
- **Description**: Consensus RPC endpoint availability status (1.0 = available, 0.0 = unavailable)

### `ev_metrics_consensus_rpc_endpoint_errors_total`
- **Type**: Counter
- **Labels**: `chain_id`, `endpoint`, `error_type`
- **Description**: Total number of consensus RPC endpoint errors by type

## Debug Logging

Debug logging provides detailed visibility into DA submission process. Enable with `--debug` flag or `EVMETRICS_DEBUG=true` environment variable.

**Use cases**: Troubleshoot submission failures, verify submission flow, diagnose Celestia RPC issues.

**Logs include**: Successful/failed submissions, DA height updates, submission timing.

**Example**:
```bash
# Enable debug logging
export EVMETRICS_DEBUG=true
./ev-metrics monitor --header-namespace testnet_header --data-namespace testnet_data
```
