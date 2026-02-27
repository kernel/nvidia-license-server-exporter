# nvidia-license-server-exporter

A Prometheus exporter for NVIDIA Cloud License Service (CLS).

It collects:
- Virtual group entitlement totals (`total`)
- Per-server feature capacity and active lease counts
- License server metadata (`info`)

The exporter is intentionally scoped to CLS only (no DLS support).

## Required credentials and IDs

- API key with `Licensing State` access
- Org ID/name (for example: `lic-...`)

Optional:
- Service instance ID (`x-nv-service-instance-id`) for environments that require it

## Configuration

Environment variables:

- `NVIDIA_API_KEY` (required)
- `NVIDIA_ORG_NAME` (required)
- `NVIDIA_API_BASE_URL` (optional, default `https://api.licensing.nvidia.com`)
- `NVIDIA_SERVICE_INSTANCE_ID` (optional)
- `LISTEN_ADDRESS` (optional, default `:9844`)
- `METRICS_PATH` (optional, default `/metrics`)
- `SCRAPE_TIMEOUT` (optional, default `20s`)
- `CACHE_TTL` (optional, default `60s`)
- `PARALLELISM` (optional, default `8`)

Flags are also available with the same names in `-kebab-case`, e.g. `-nvidia-org-name`.

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

## Exported metrics

Core health:

- `nvidia_cls_up`
- `nvidia_cls_scrape_duration_seconds`
- `nvidia_cls_scrape_timestamp_seconds`

Entitlement:

- `nvidia_cls_entitlement_total_quantity`

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
