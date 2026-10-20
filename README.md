# EV Metrics

Data Availability metrics and monitoring tool for evstack using Celestia DA. This tool monitors EVM block headers in real-time, queries ev-node for DA submission information, and verifies blob data on Celestia.

## Features

- Real-time EVM block header streaming
- Automatic verification of header and data blobs on Celestia
- Retry logic with exponential backoff for pending DA submissions
- Prometheus metrics for tracking unverified block ranges
- Streaming block verification of every new block
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


### Prometheus Metrics Endpoint

The Prometheus metrics server always runs. Use `--port` to change the port:

```bash
./ev-metrics monitor \
  --header-namespace collect_testnet_header \
  --data-namespace collect_testnet_data \
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
- `--celestia-token`: Celestia authentication token (optional), sent as a `Bearer` token on all Celestia requests, including block timestamp lookups. Required when the celestia-node has auth enabled (the default).
- `--duration`: Duration in seconds to stream (0 = infinite)
- `--chain-id`: Chain identifier for metrics labels (default: "testnet")
- `--port`: HTTP server port for metrics (default: 2112)
- `--jsonrpc-scrape-interval`: JSON-RPC health check scrape interval in seconds (default: 10)
- `--reference-node`: Reference node RPC endpoint URL (sequencer) for drift monitoring
- `--full-nodes`: Comma-separated list of full node RPC endpoint URLs for drift monitoring
- `--polling-interval`: Polling interval in seconds for checking node block heights (default: 10)
- `--balance.addresses`: Comma-separated Celestia addresses to monitor (enables balance checking)
- `--balance.consensus-rpc-urls`: Comma-separated consensus RPC URLs for balance queries (required if balance.addresses is set)
- `--balance.scrape-interval`: Balance check scrape interval in seconds (default: 30)
- `--verifier.workers`: Number of concurrent workers for block verification (default: 50)
- `--health-max-header-age`: Seconds without a new block header after which `/health` returns 503 (default: 0, which disables the age check so only the subscription state matters). See [Health Endpoint](#health-endpoint).
- `--verbose`: Enable verbose (debug level) logging (default: false)
- `--log-format`: Log output format, `console` (human-readable) or `json` (one JSON object per line, for Loki/ELK ingestion) (default: `console`)

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
  --balance.scrape-interval 30
```

This will query account balances every 30 seconds with automatic failover between RPC endpoints.

`https://` consensus endpoints are dialed over TLS (port 443 if none is given); `http://` and bare `host:port` endpoints use plaintext gRPC.

## Prometheus Metrics

The following metrics are exposed:

Blocks without transactions have only their header blob verified (`type="header"`). Blocks with transactions have both their header blob (`type="header"`) and data blob (`type="data"`) verified, and each outcome is recorded under its own type. Tx blocks now count under `type="header"` too, so header submission counts (`submission_*`, `unsubmitted_*`, DA height and duration) rise after upgrading. Each block costs one extra Celestia request per transaction block for the header blob. If retries are exhausted, only the blob types that are still unverified are recorded as missing.

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
- **Description**: Delay between block creation (`header.Time`) and reception with histogram buckets. Only headers received from the subscription are observed (not backfilled ones), and negative values from clock skew are recorded as 0. `header.Time` has 1-second resolution, so the value is only accurate to about ±1s.
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

### Verifier Self-Observability Metrics

### `ev_metrics_verifier_queue_length`
- **Type**: Gauge
- **Labels**: `chain_id`
- **Description**: Number of blocks waiting in the verifier queue, sampled every 10 seconds. A value that stays near the queue capacity (2 x `--verifier.workers`) means the verifier is falling behind.

### `ev_metrics_verifier_inflight_blocks`
- **Type**: Gauge
- **Labels**: `chain_id`
- **Description**: Blocks between their first verification attempt and being verified or running out of retries, including blocks waiting on a retry timer. A block re-queued after the cooldown is counted again.

### `ev_metrics_ws_reconnects_total`
- **Type**: Counter
- **Labels**: `chain_id`
- **Description**: Number of times the block header WebSocket subscription was re-established after dropping.

### `ev_metrics_last_header_received_timestamp`
- **Type**: Gauge
- **Labels**: `chain_id`
- **Description**: Unix timestamp in seconds of the last block header received from the subscription (backfilled headers are not counted). It is 0 until the first header arrives.

### `ev_metrics_verifier_backfilled_blocks_total`
- **Type**: Counter
- **Labels**: `chain_id`
- **Description**: Blocks enqueued after a gap in the header subscription (for example after a reconnect).

### `ev_metrics_verifier_backfill_dropped_total`
- **Type**: Counter
- **Labels**: `chain_id`
- **Description**: Blocks skipped because a subscription gap was larger than the 1000-block backfill cap. The oldest blocks are dropped.

### Health Endpoint

`GET /health` returns `200 OK`, or `503` with a short plain-text reason when:
- the block header subscription has not been established yet (`not ready`),
- the subscription is down (it is re-established automatically with backoff), or
- `--health-max-header-age` is greater than 0 and no header has arrived within that many seconds (measured from the first subscription until a header arrives).

Use `/health` as a **readiness** probe, not a liveness probe. With the age check enabled, a halted chain also fails it, and restarting the monitor would not fix that.

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

Endpoint labels (here and in the balance metrics below) and logs show only `scheme://host[:port]`, so credentials in endpoint URLs (userinfo, path, query) are not exposed. Endpoints that redact to the same value get a `#<index>` suffix to stay distinct.

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

There are two independent debug logging mechanisms:

- `--verbose` flag: sets the application log level to debug. Per-block and per-poll success messages (block verification, drift polls, balance checks, JSON-RPC health checks) are logged at debug level, so use `--verbose` to see them. Warnings and errors are always logged.
- `EVMETRICS_DEBUG=true` environment variable: logs DA submission details from the metrics package.

**Use cases**: Troubleshoot submission failures, verify submission flow.

**`EVMETRICS_DEBUG` logs include**: Successful/failed submission attempts and DA height updates.

**Example**:
```bash
# Enable debug logging
export EVMETRICS_DEBUG=true
./ev-metrics monitor --header-namespace testnet_header --data-namespace testnet_data
```
