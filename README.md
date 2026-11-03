# nvidia-license-server-exporter

A NVIDIA Cloud License Service (CLS) exporter with:
- Prometheus pull (`/metrics`)
- Optional OTEL metrics push (OTLP gRPC)

The exporter is intentionally scoped to CLS only (no DLS support).

## Required credentials and IDs

- API key with `Licensing State` access
- Org ID/name (for example: `lic-...`)

Optional:
- Service instance ID (`x-nv-service-instance-id`) for environments that require it

## NVIDIA API endpoints

The exporter reads the supported listing endpoints of the NVIDIA Licensing Portal API:

- `GET /v1/org/{org}/virtual-groups` for entitlement features
- `GET /v1/org/{org}/virtual-groups/{vg}/allocations` (paginated) for license servers and their features
- `GET /v1/org/{org}/virtual-groups/{vg}/leases` (paginated, filtered by `license_server_ids`, scoped by `X-NV-SERVICE-INSTANCE-ID`) for active leases

Lease lookups skip network-license allocations and explicitly non-cloud deployments, even when `NVIDIA_SERVICE_INSTANCE_ID` is configured. Their metadata and capacity are still exported.

NVIDIA retired `.../license-servers/{id}/license-pools` (it now answers HTTP 400 "This API is no longer supported") and deprecated `.../license-servers` in favor of allocations; neither is used.

## Configuration

### CLS and server

- `NVIDIA_API_KEY` (required)
- `NVIDIA_ORG_NAME` (required)
- `NVIDIA_API_BASE_URL` (optional, default `https://api.licensing.nvidia.com`)
- `NVIDIA_SERVICE_INSTANCE_ID` (optional)
- `LISTEN_ADDRESS` (optional, default `:9844`)
- `METRICS_PATH` (optional, default `/metrics`)
- `SCRAPE_TIMEOUT` (optional, default `20s`)
- `CACHE_TTL` (optional, default `60s`)
- `PARALLELISM` (optional, default `8`)

If `LISTEN_ADDRESS` is unset and `PORT` is set (for example on Railway), the exporter listens on `:$PORT`.

### OTEL push (optional)

- `OTEL_ENABLED` (optional, default `false`)
- `OTEL_ENDPOINT` (optional, default `127.0.0.1:4317`)
- `OTEL_SERVICE_NAME` (optional, default `nvidia-license-server-exporter`)
- `OTEL_SERVICE_INSTANCE_ID` (optional, default hostname)
- `OTEL_INSECURE` (optional, default `true`)
- `OTEL_PUSH_INTERVAL` (optional, default `60s`)
- `OTEL_RESOURCE_ATTRIBUTES` (optional, standard OpenTelemetry resource attributes such as `deployment.environment=production`)

Flags are also available in `-kebab-case` (for example `-otel-enabled`, `-otel-endpoint`).

## Run

```bash
go mod tidy
go run ./cmd/nvidia-license-server-exporter \
  -nvidia-api-key "$NVIDIA_API_KEY" \
  -nvidia-org-name "$NVIDIA_ORG_NAME"
```

Endpoints:

- `GET /metrics`
- `GET /healthz`

## Shared cache behavior

Prometheus pull and OTEL push use the same snapshot cache.

- If cache is fresh (`CACHE_TTL`), no CLS API call is made.
- If cache is stale, one refresh call updates cache for both pull and push.
- If refresh fails and a stale snapshot exists, stale data is still emitted with `nvidia_cls_up=0` and `nvidia_cls_scrape_timestamp_seconds` keeps the last successful collection time.
- If refresh fails before any snapshot exists, `nvidia_cls_up=0` and `nvidia_cls_scrape_duration_seconds` are still emitted; the timestamp and data metrics appear once a snapshot is collected.

`/healthz` only reports that the process serves HTTP. Alert on `nvidia_cls_up` for collection health.

Recommended default:
- `CACHE_TTL=60s`
- `OTEL_PUSH_INTERVAL=60s`

## Exported metrics

Core health:

- `nvidia_cls_up`
- `nvidia_cls_scrape_duration_seconds`
- `nvidia_cls_scrape_timestamp_seconds`

Entitlement (one series per entitlement product key, labeled by `ems_entitlement_id` and `ems_product_key_id`, so same-product entitlements purchased separately stay distinct; sum across series for the org-wide total):

- `nvidia_cls_entitlement_total_quantity`
- `nvidia_cls_entitlement_in_use_quantity`
- `nvidia_cls_entitlement_unassigned_quantity`

Server:

- `nvidia_cls_license_server_info`
- `nvidia_cls_license_server_feature_total_quantity`
- `nvidia_cls_license_server_feature_active_leases`

All metrics include constant label `org_name="<your org id>"`.

## Prometheus scrape config example

```yaml
scrape_configs:
  - job_name: nvidia_cls
    static_configs:
      - targets:
          - localhost:9844
```
