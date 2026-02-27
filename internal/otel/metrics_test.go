package otel

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
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
				VirtualGroupID:   101,
				VirtualGroupName: "VG",
				FeatureName:      "Feature A",
				FeatureVersion:   "1.0",
				ProductName:      "Product",
				LicenseType:      "TYPE",
				TotalQuantity:    128,
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
		ServerUsage: []cls.ServerUsageSnapshot{
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
	if len(obs) != 7 {
		t.Fatalf("expected 7 observations, got %d", len(obs))
	}

	counts := make(map[string]int)
	for _, o := range obs {
		counts[o.name]++
		attrs := attrMap(o.attrs)
		if attrs["org_name"] != "org-1" {
			t.Fatalf("observation %s missing org_name attribute", o.name)
		}
	}

	if counts[metricUp] != 1 ||
		counts[metricScrapeDuration] != 1 ||
		counts[metricScrapeTimestamp] != 1 ||
		counts[metricEntitlementTotal] != 1 ||
		counts[metricServerFeatureTotal] != 1 ||
		counts[metricServerFeatureActive] != 1 ||
		counts[metricServerInfo] != 1 {
		t.Fatalf("unexpected observation counts: %+v", counts)
	}
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
