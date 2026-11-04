package otel

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"nvidia-license-server-exporter/internal/cls"
	"nvidia-license-server-exporter/internal/snapshot"
)

func TestNormalizeConfigDefaults(t *testing.T) {
	cfg := normalizeConfig(Config{})
	if cfg.PushInterval != defaultPushInterval {
		t.Fatalf("expected default push interval %s, got %s", defaultPushInterval, cfg.PushInterval)
	}
	if cfg.RefreshTimeout != defaultRefreshTimeout {
		t.Fatalf("expected default refresh timeout %s, got %s", defaultRefreshTimeout, cfg.RefreshTimeout)
	}
}

func TestNewResourceHonorsEnvironmentAttributes(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment=production,service.name=ignored")

	res, err := newResource(context.Background(), Config{ServiceName: "svc", ServiceInstanceID: "task-1"})
	if err != nil {
		t.Fatalf("new resource: %v", err)
	}
	attrs := attrMap(res.Attributes())
	if attrs["deployment.environment"] != "production" {
		t.Fatalf("deployment.environment = %q, want production", attrs["deployment.environment"])
	}
	if attrs["service.name"] != "svc" || attrs["service.instance.id"] != "task-1" {
		t.Fatalf("configured service identity must win: %v", attrs)
	}
}

func TestBuildObservationsMapping(t *testing.T) {
	ts := time.Unix(1700000000, 0).UTC()
	meta := snapshot.Meta{
		Up:              1,
		DurationSeconds: 1.25,
		Timestamp:       ts,
	}
	snap := &cls.Snapshot{
		EntitlementFeatures: []cls.EntitlementFeatureSnapshot{
			{
				VirtualGroupID:     101,
				VirtualGroupName:   "VG",
				EMSEntitlementID:   "ent-1",
				EMSProductKeyID:    "pk-1",
				FeatureName:        "Feature A",
				FeatureVersion:     "1.0",
				ProductName:        "Product",
				LicenseType:        "TYPE",
				TotalQuantity:      128,
				InUseQuantity:      53,
				UnassignedQuantity: 75,
			},
		},
		ServerFeatureCapacity: []cls.ServerFeatureCapacitySnapshot{
			{
				VirtualGroupID:   101,
				VirtualGroupName: "VG",
				ServerID:         "srv-1",
				ServerName:       "server-1",
				FeatureName:      "Feature A",
				ProductName:      "Product",
				LicenseType:      "TYPE",
				TotalQuantity:    128,
			},
		},
		ServerFeatureActiveLeases: []cls.ServerFeatureActiveLeaseSnapshot{
			{
				VirtualGroupID:   101,
				VirtualGroupName: "VG",
				ServerID:         "srv-1",
				ServerName:       "server-1",
				FeatureName:      "Feature A",
				ProductName:      "Product",
				LicenseType:      "TYPE",
				ActiveLeases:     19,
			},
		},
		ServerInfo: []cls.ServerInfoSnapshot{
			{
				VirtualGroupID:   101,
				VirtualGroupName: "VG",
				ServerID:         "srv-1",
				ServerName:       "server-1",
				ServerStatus:     "ENABLED",
				DeployedOn:       "CLOUD",
				LeasingMode:      "STANDARD",
			},
		},
	}

	obs := buildObservations("org-1", snap, meta)
	if len(obs) != 9 {
		t.Fatalf("expected 9 observations, got %d", len(obs))
	}

	counts := make(map[string]int)
	for _, o := range obs {
		counts[o.name]++
		attrs := attrMap(o.attrs)
		if attrs["org_name"] != "org-1" {
			t.Fatalf("observation %s missing org_name attribute", o.name)
		}
		switch o.name {
		case metricEntitlementTotal, metricEntitlementInUse, metricEntitlementUnassigned:
			if attrs["ems_entitlement_id"] != "ent-1" || attrs["ems_product_key_id"] != "pk-1" {
				t.Fatalf("observation %s missing entitlement id attributes: %v", o.name, attrs)
			}
		}
	}

	if counts[metricUp] != 1 ||
		counts[metricScrapeDuration] != 1 ||
		counts[metricScrapeTimestamp] != 1 ||
		counts[metricEntitlementTotal] != 1 ||
		counts[metricEntitlementInUse] != 1 ||
		counts[metricEntitlementUnassigned] != 1 ||
		counts[metricServerFeatureTotal] != 1 ||
		counts[metricServerFeatureActive] != 1 ||
		counts[metricServerInfo] != 1 {
		t.Fatalf("unexpected observation counts: %+v", counts)
	}
}

func TestBuildObservationsWithoutSnapshot(t *testing.T) {
	obs := buildObservations("org-1", nil, snapshot.Meta{DurationSeconds: 0.5})

	values := make(map[string]float64, len(obs))
	for _, o := range obs {
		values[o.name] = o.value
	}
	if len(values) != 2 || values[metricScrapeDuration] != 0.5 {
		t.Fatalf("observations without a snapshot = %v, want only health", values)
	}
	if up, ok := values[metricUp]; !ok || up != 0 {
		t.Fatalf("up = %v, want 0", values[metricUp])
	}
}

func TestBuildObservationsStaleSnapshotKeepsTimestamp(t *testing.T) {
	collected := time.Unix(1700000000, 0).UTC()
	obs := buildObservations("org-1", &cls.Snapshot{CollectedAt: collected}, snapshot.Meta{Up: 0, Timestamp: collected})

	values := make(map[string]float64, len(obs))
	for _, o := range obs {
		values[o.name] = o.value
	}
	if values[metricUp] != 0 || values[metricScrapeTimestamp] != float64(collected.Unix()) {
		t.Fatalf("stale observations = %v, want down with the last collection time", values)
	}
}

type flakyFetcher struct {
	fail bool
}

func (f *flakyFetcher) FetchSnapshot(context.Context) (*cls.Snapshot, error) {
	if f.fail {
		return nil, errors.New("request failed with status 400")
	}
	return &cls.Snapshot{CollectedAt: time.Now().UTC()}, nil
}

func TestRegisteredMetricsReportFailedColdStart(t *testing.T) {
	ctx := context.Background()
	fetcher := &flakyFetcher{fail: true}
	svc := snapshot.NewService(fetcher, time.Minute)
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = provider.Shutdown(ctx) }()

	pusher := &MetricsPusher{orgName: "org-1", snapshotSvc: svc}
	if err := pusher.registerMetrics(provider.Meter("test")); err != nil {
		t.Fatalf("register metrics: %v", err)
	}

	pusher.refreshOnce()
	gauges := collectGauges(t, ctx, reader)
	if up, ok := gauges[metricUp]; !ok || up != 0 {
		t.Fatalf("failed cold start must report up=0, got %v", gauges)
	}
	if _, ok := gauges[metricScrapeTimestamp]; ok {
		t.Fatalf("failed cold start must not report a snapshot timestamp: %v", gauges)
	}

	fetcher.fail = false
	pusher.refreshOnce()
	gauges = collectGauges(t, ctx, reader)
	if gauges[metricUp] != 1 || gauges[metricScrapeTimestamp] == 0 {
		t.Fatalf("recovered collection must report up=1 with a timestamp, got %v", gauges)
	}
}

func collectGauges(t *testing.T, ctx context.Context, reader *sdkmetric.ManualReader) map[string]float64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	gauges := make(map[string]float64)
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			gauge, ok := m.Data.(metricdata.Gauge[float64])
			if !ok || len(gauge.DataPoints) != 1 {
				t.Fatalf("metric %s: unexpected data %+v", m.Name, m.Data)
			}
			gauges[m.Name] = gauge.DataPoints[0].Value
		}
	}
	return gauges
}

func TestNewMetricsPusherValidation(t *testing.T) {
	svc := snapshot.NewService(&testFetcher{}, time.Minute)

	if _, err := NewMetricsPusher(context.Background(), Config{
		ServiceName: "svc",
	}, "org", svc); err == nil {
		t.Fatalf("expected error for missing endpoint")
	}

	if _, err := NewMetricsPusher(context.Background(), Config{
		Endpoint: "127.0.0.1:4317",
	}, "org", svc); err == nil {
		t.Fatalf("expected error for missing service name")
	}
}

func attrMap(attrs []attribute.KeyValue) map[string]string {
	out := make(map[string]string, len(attrs))
	for _, kv := range attrs {
		out[string(kv.Key)] = kv.Value.AsString()
	}
	return out
}

type testFetcher struct{}

func (t *testFetcher) FetchSnapshot(context.Context) (*cls.Snapshot, error) {
	return &cls.Snapshot{CollectedAt: time.Now().UTC()}, nil
}
