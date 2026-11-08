# Time API

A small self-hosted REST API for UTC time, timezone data, published DUT1
(UT1−UTC) values, and observe-only upstream NTP health. It is designed for the
Time Station Simulator but can be used by any client that needs the same JSON
fields.

The API does not set or discipline the host clock. It reports time and clock
health; the only timestamp adjustment it can make is a bounded correction
applied to the `/v1/time/ntp` response while `NTP_CORRECTION_MODE=first`.

This project is based on [petstack/time-api](https://github.com/petstack/time-api)
and adds DUT1 provisioning and the upstream NTP monitor.

## Features

- Current time in several formats and selectable timezones
- `/v1/time/ntp`: compact endpoint with microsecond server timestamps for
  browser four-timestamp synchronization
- Observe-only upstream NTP monitor: reports reachability, offset, stratum,
  quality gates and holdover without changing the host clock
- DUT1 endpoint backed by IERS Bulletin A and the NTSC EOP product, with
  in-memory caching and observed/predicted value types
- OpenAPI document and timezone list
- CORS for GET requests
- JSON request logs with request IDs

## Quick start

### Docker

```bash
docker build -t time-api .
docker run -d --name time-api -p 8080:8080 \
  -e TZ=UTC \
  -e NTP_MONITOR_SERVERS=pool.ntp.org \
  time-api
```

Or edit `docker-compose.yml` and run:

```bash
docker compose up -d --build
```

The API then listens at `http://localhost:8080`. The compose file publishes host
port `8080` by default; set `TIME_API_PORT` to use another host port. Put the API
behind an HTTPS reverse proxy for public use; do not expose the container port
directly to the internet.

### Local Go build

```bash
go mod download
go run .
```

Go 1.21 or newer is required.

## Endpoints

### `GET /v1/time`

Returns the current time. `timezone` is optional and defaults to `UTC`.

```text
GET /v1/time?timezone=UTC
```

The response includes the usual date/time fields and a `dut1` object:

```json
{
  "timestamp": 1792310942,
  "time": "08:09:02.214457",
  "date": "2026-10-18",
  "timezone": "UTC",
  "abbr": "UTC",
  "offset": "+00:00",
  "iso8601": "2026-10-18T08:09:02Z",
  "server_received_ms": 1792310942121,
  "server_sent_ms": 1792310942214,
  "dut1": {
    "date": "2026-10-18",
    "date_basis": "UTC",
    "unit": "s",
    "source": "IERS Bulletin A",
    "source_key": "iers",
    "source_mode": "auto",
    "stale": false,
    "available": true,
    "previous": { "date": "2026-10-17", "mjd": 61330, "value": -0.03769, "dut1": 0, "type": "predicted" },
    "current":  { "date": "2026-10-18", "mjd": 61331, "value": -0.03825, "dut1": 0, "type": "predicted" },
    "next":     { "date": "2026-10-19", "mjd": 61332, "value": -0.03897, "dut1": 0, "type": "predicted" }
  }
}
```

`server_received_ms` and `server_sent_ms` are UTC Unix milliseconds captured at
handler entry and immediately before writing the response. A client can combine
them with its own send/receive timestamps to estimate clock offset without
assuming that server processing time is negligible.

`/v1/time` always reports raw host time; `clock_source` is `host`. Use
`/v1/time/ntp` when clock-health metadata or the optional bounded correction is
required.

### `GET /v1/time/ntp`

Compact endpoint intended for browser synchronization:

```text
GET /v1/time/ntp
```

The response includes `server_received_unix_us`, `server_sent_unix_us`, the
corresponding millisecond fields, a `sequence` number, and `clock_health`:

```json
{
  "sequence": 12,
  "server_received_unix_us": 1792310942123456,
  "server_sent_unix_us": 1792310942123789,
  "clock_health": {
    "enabled": true,
    "server": "pool.ntp.org",
    "reachable": true,
    "synchronized": true,
    "holdover": false,
    "has_sample": true,
    "offset_ok": true,
    "quality_ok": true,
    "offset_ms": 1.8,
    "uncertainty_ms": 6.2,
    "rtt_ms": 12.4,
    "stratum": 2,
    "leap": 0,
    "root_delay_ms": 18.2,
    "root_dispersion_ms": 2.1,
    "last_success_at": "2026-10-18T08:08:58Z",
    "age_s": 4.2
  }
}
```

The monitor performs a direct SNTP query from inside the API process. It does
not read the host NTP daemon and never changes the host clock.

With `NTP_CORRECTION_MODE=first` (default), the endpoint may serve corrected
`server_*` timestamps when the first configured source is reachable, fully
synchronized, not a fallback, fresh, and within the configured uncertainty
gates. Fallback sources are observation-only. Raw host timestamps remain in
`host_*` fields. Set `NTP_CORRECTION_MODE=off` to disable correction entirely.

Correction metadata is explicit:

```text
clock_correction_applied
clock_correction_eligible
clock_correction_ms
clock_correction_mode       first | off
clock_correction_source
clock_correction_reason     primary_synchronized | fallback_source | offset_gate | ...
clock_correction_uncertainty_ms
clock_source                host | host+first-source
```

The temporary probe below performs one query without changing the monitor or
the host clock:

```text
GET /v1/time/ntp?server=time.cloudflare.com
```

The `server` value must be a public DNS name. IP literals, localhost, private
suffixes/resolved addresses and unusual targets are rejected. Probes are
rate-limited. To force the configured monitor to poll immediately:

```text
GET /v1/time/ntp?refresh=1
```

`refresh=1` and `server=` are mutually exclusive.

### `GET /v1/dut1`

Returns UT1−UTC for the requested UTC date and its neighbours:

```text
GET /v1/dut1?date=2026-10-18&source=auto
```

`date` is optional; the current UTC date is used when omitted. `source` accepts
`auto` (default), `ntsc`, or `iers`. `auto` prefers a fresh NTSC EOP cache and
falls back to IERS Bulletin A.

Values are in seconds. `dut1` is the same value rounded to the 0.1 s step used by
time-signal broadcasts. `type` is `observed` or `predicted`. If no value exists
for the requested date, the endpoint still returns HTTP 200 with
`"available": false` and an `error` field.

### `GET /v1/timezones`

Lists available timezones.

### `GET /v1/openapi.json`

Returns the OpenAPI document.

## Configuration

The server listens on `8080` inside the container. All settings are optional
environment variables.

### DUT1 provider

| Variable | Default | Description |
| --- | --- | --- |
| `DUT1_SOURCE_URL` | `https://datacenter.iers.org/data/latestVersion/bulletinA.txt` | Upstream EOP source. A comma-separated list is tried as fallback sources. |
| `DUT1_REFRESH_INTERVAL` | `6h` | Cache refresh interval; minimum 5 minutes. |
| `DUT1_HTTP_TIMEOUT` | `20s` | HTTP timeout for one source; minimum 5 seconds. |
| `DUT1_STALE_AFTER` | `2 × refresh` | Age after which cached data is reported as stale. |
| `DUT1_NTSC_ENABLED` | `true` | Also cache the NTSC EOP product. |
| `DUT1_NTSC_BASE_URL` | `http://www.ut1.ntsc.ac.cn` | NTSC EOP site base URL. |
| `DUT1_DEFAULT_SOURCE` | `auto` | Default source: `auto`, `ntsc`, or `iers`. |

The parser recognizes IERS Bulletin A and 16-column NTSC EOP files. NTSC is not
a single static file: the service queries the site's data list, selects the
newest `NTSC_EOP_yyyyMMdd` file, and downloads it. The parser uses the file's
`Last date with real data` marker to distinguish observed rows from predictions.

### Upstream NTP monitor

| Variable | Default | Description |
| --- | --- | --- |
| `NTP_MONITOR_ENABLED` | `true` | Enable the observe-only upstream query. |
| `NTP_MONITOR_SERVERS` | `ntp.ntsc.ac.cn` | Comma-separated priority list. The first entry is the correction authority in `first` mode. Each DNS name is expanded to all of its addresses. |
| `NTP_MONITOR_SERVER` | — | Backward-compatible single-source alias used when `NTP_MONITOR_SERVERS` is empty. |
| `NTP_MONITOR_INTERVAL` | `5m` | Query interval; minimum 30 seconds. |
| `NTP_MONITOR_TIMEOUT` | `3s` | Per-query UDP timeout; minimum 1 second. |
| `NTP_MONITOR_HOLDOVER_AFTER` | `max(3 × interval, 20m)` | Age after which a cached sample is marked as holdover. |
| `NTP_MONITOR_MAX_OFFSET_MS` | `10` | Absolute upstream offset gate for `synchronized`. |
| `NTP_MONITOR_MAX_UNCERTAINTY_MS` | `20` | Uncertainty gate; above this `quality_ok` and `synchronized` are false. |
| `NTP_CORRECTION_MAX_UNCERTAINTY_MS` | `20` | Maximum effective uncertainty for correction. |
| `NTP_CORRECTION_MAX_AGE` | `10m` | Maximum age of a sample used for correction. |
| `NTP_CORRECTION_MODE` | `first` | `first` allows only the first configured source, while synchronized and non-fallback, to correct. `off` disables correction. |
| `NTP_CORRECTION_DRIFT_MS_PER_HOUR` | `0` | Optional uncertainty growth per hour of sample age. |

Sources are tried left to right. The monitor reports the first source whose
`offset_ok` and `quality_ok` gates pass; if none pass, it reports the
highest-priority reachable source for health only. `clock_health.fallback` is
true when the reported source is not the first configured one.
`clock_health.sources` contains the latest per-source observations, including
`server`, `resolved_ip`, `reachable`, `selected`, `offset_ms`, `uncertainty_ms`,
`rtt_ms`, `stratum`, `offset_ok`, `quality_ok`, `root_delay_ms`,
`root_dispersion_ms`, `leap`, timestamps, `age_s`, `consecutive_failures`,
`reach` and `reach_octal`.

A sample priority list:

```text
pool.ntp.org,time.cloudflare.com
```

The container needs outbound DNS and UDP/123 for the monitor, plus outbound
HTTP(S) for DUT1 data. It does not need root, `CAP_NET_ADMIN`, or access to the
host NTP daemon.

## Tests and development

```bash
go test ./...
go run .
```

The project includes unit tests for the NTP monitor and DUT1 parser. The server
has no build-time frontend dependency; the API is a single Go binary.

## Deployment notes

- The API returns public data but has no authentication. Restrict access with a
  firewall or reverse proxy as appropriate.
- Use HTTPS when the web page is served over HTTPS; mixed content is blocked by
  browsers.
- CORS is intentionally open for GET requests so browser clients can read the
  public endpoints.

## License

MIT — see [LICENSE](LICENSE).

[![](https://img.shields.io/badge/powered_by-dsh-4D6BFE?style=flat-square&logo=deepseek&logoColor=white)](https://github.com/deepseek-ai/deepseek-harness)
