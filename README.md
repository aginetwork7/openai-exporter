[![Docker Pulls](https://img.shields.io/docker/pulls/foxdalas/openai-exporter?style=flat-square)](https://hub.docker.com/r/foxdalas/openai-exporter) [![GitHub Release](https://img.shields.io/github/v/release/foxdalas/openai-exporter?style=flat-square)](https://github.com/foxdalas/openai-exporter/releases)

# OpenAI Exporter

This is an OpenAI Prometheus Exporter designed to fetch and expose usage data metrics from OpenAI's APIs. The exporter provides metrics related to the utilization of various OpenAI services, such as completions, embeddings, and moderations, suitable for monitoring and analytics purposes.

## Features

- Fetches usage and cost data from OpenAI APIs.
- Exposes metrics in Prometheus format.
- Configurable metrics collection intervals.
- Automatic project name resolution and enrichment.
- Supports multiple OpenAI service endpoints:
  - Completions
  - Embeddings
  - Moderations
  - Images
  - Audio Speeches
  - Audio Transcriptions
  - Vector Stores
- Current-day cost tracking with multi-currency support.

## Prerequisites

- Go 1.x or higher
- Access to OpenAI API and organization IDs

## Configuration

Before running the exporter, ensure the following environment variables are set:
- `OPENAI_SECRET_KEY`: Your OpenAI API secret key, this can be [admin read only key](https://platform.openai.com/settings/organization/admin-keys)
- `OPENAI_ORG_ID`: Your organization ID with OpenAI, you can see it [here](https://platform.openai.com/settings/organization/general)

## Building binary

```shell
go build -o openai-exporter
```

## Usage

```shell
./openai-exporter
```

### Docker

```shell
docker run -d -p 9185:9185 -e OPENAI_SECRET_KEY=your_secret_key -e OPENAI_ORG_ID=your_org_id foxdalas/openai-exporter:v0.0.16
```

Use the following flags to customize the behavior:

* `-web.listen-address`: Set the listen address for the web interface and telemetry (default: `:9185`).
* `-web.telemetry-path`: Set the path under which to expose metrics (default: `/metrics`).
* `-scrape.interval`: Set the interval for API calls and the size of each usage window (default: `1m`). Must be a whole number of seconds.
* `-scrape.settle-delay`: How long after a usage window has ended before it is fetched, because OpenAI usage buckets can arrive late (default: `1m`).
* `-scrape.max-catchup-windows`: Maximum number of usage windows fetched in one collection cycle when the collector is behind (default: `60`).
* `-scrape.catchup-spacing`: Minimum time between the starts of consecutive usage window fetches; keeps catch-up under the OpenAI rate limit (default: `20s`, must not exceed `-scrape.interval`).
* `-cost.interval`: Interval for refreshing today's cost (default: `5m`).
* `-http.timeout`: Timeout for each OpenAI API request (default: `30s`).
* `-log.level`: Set the log verbosity (default: `info`).

## How It Works

### Token Metrics Collection

- Fetches usage data every minute (configurable via `-scrape.interval`)
- Windows are aligned to the wall clock: a window `[t, t+interval)` is fetched once `now >= t + interval + settle-delay`, so the collector does not fall behind however long API calls take
- If the collector is behind (slow API calls, outages), missed windows are fetched oldest first without waiting for the next interval, at most `-scrape.max-catchup-windows` per cycle and spaced by `-scrape.catchup-spacing`
- Each window queries 7 usage endpoints. OpenAI organization endpoints are limited to about 30 requests per minute, so the default 20s spacing allows at most 3 windows (21 requests) per minute while catching up, leaving room for cost and name lookups
- Collects data in 1-minute buckets with automatic deduplication
- Aggregates metrics by model, operation, project, user, API key, and batch status
- Only processes completed time buckets to ensure data accuracy

### Cost Metrics Collection

- Fetches current-day cost data every `-cost.interval`, independently of the usage windows: `start_time` is today 00:00 UTC, `end_time` is now, `bucket_width=1d`
- Only today's UTC bucket is exported; at the UTC day change the metric is reset, so projects with no spend today disappear
- If a cost request fails, the previous values are kept
- Groups today's costs by project with line-item breakdown
- Supports multiple currencies (indicated by the `currency` label)

### Project Name Enrichment

- Automatically resolves project IDs to human-readable names
- Caches project names to minimize API calls
- Falls back to "unknown" if project name cannot be resolved

## Metrics Examples

The exporter provides two metrics:

### `openai_api_tokens_total`

Counter metric tracking token usage across all operations.

**Labels:**
- `model`: OpenAI model name (e.g., `gpt-4-turbo-2024-04-09`)
- `operation`: API operation type (e.g., `completions`, `embeddings`)
- `project_id`: OpenAI project identifier
- `project_name`: Human-readable project name (auto-resolved)
- `user_id`: User identifier
- `api_key_id`: API key identifier
- `batch`: Whether the request was batched (`true`/`false`)
- `token_type`: Type of tokens (`input`, `output`, `input_cached`, `input_audio`, `output_audio`)

### `openai_api_today_cost`

Gauge metric tracking today's costs per project.

**Labels:**
- `project_id`: OpenAI project identifier
- `project_name`: Human-readable project name (auto-resolved)
- `line_item`: Cost line item description
- `organization_id`: OpenAI organization identifier
- `currency`: Currency code (for example, `usd`)

### Example Output

```promql
openai_api_tokens_total{api_key_id="",batch="false",model="gpt-4-turbo-2024-04-09",operation="completions",project_id="",project_name="production",token_type="input",user_id=""} 1081
openai_api_tokens_total{api_key_id="",batch="false",model="gpt-4-turbo-2024-04-09",operation="completions",project_id="",project_name="production",token_type="input_audio",user_id=""} 0
openai_api_tokens_total{api_key_id="",batch="false",model="gpt-4-turbo-2024-04-09",operation="completions",project_id="",project_name="production",token_type="input_cached",user_id=""} 0
openai_api_tokens_total{api_key_id="",batch="false",model="gpt-4-turbo-2024-04-09",operation="completions",project_id="",project_name="production",token_type="output",user_id=""} 1432
openai_api_tokens_total{api_key_id="",batch="false",model="gpt-4-turbo-2024-04-09",operation="completions",project_id="",project_name="production",token_type="output_audio",user_id=""} 0
openai_api_today_cost{currency="usd",line_item="GPT-4 Turbo",organization_id="org-123",project_id="proj-456",project_name="production"} 42.50
```

## Contributing

Contributions to the OpenAI Exporter are welcome. Please ensure that your commits meet the following criteria:
* Code must be well-documented.
* Commits must be clear and concise.
* All changes must be tested thoroughly.

## License

This project is licensed under the MIT License - see the [LICENSE](https://github.com/foxdalas/openai-exporter/blob/master/LICENSE) file for details.
