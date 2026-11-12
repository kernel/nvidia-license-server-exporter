package exporter

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"nvidia-license-server-exporter/internal/cls"
	"nvidia-license-server-exporter/internal/snapshot"
)

type entitlementFetcher struct {
	snapshot *cls.Snapshot
}

func (f entitlementFetcher) FetchSnapshot(context.Context) (*cls.Snapshot, error) {
	return f.snapshot, nil
}

type failingFetcher struct{}

func (failingFetcher) FetchSnapshot(context.Context) (*cls.Snapshot, error) {
	return nil, errors.New("request failed with status 400")
}

func TestCollectorReportsFailedColdStart(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(NewCollector(snapshot.NewService(failingFetcher{}, time.Minute), "org-1", time.Second))

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}

	got := make(map[string]float64, len(families))
	for _, family := range families {
		if len(family.GetMetric()) != 1 {
			t.Fatalf("family %s has %d metrics, want 1", family.GetName(), len(family.GetMetric()))
		}
		got[family.GetName()] = family.GetMetric()[0].GetGauge().GetValue()
	}
	if up, ok := got["nvidia_cls_up"]; !ok || up != 0 {
		t.Fatalf("nvidia_cls_up = %v, want 0: %v", got["nvidia_cls_up"], got)
	}
	if _, ok := got["nvidia_cls_scrape_timestamp_seconds"]; ok {
		t.Fatalf("failed cold start must not report a snapshot timestamp: %v", got)
	}
	if len(got) != 2 {
		t.Fatalf("expected only health metrics, got %v", got)
	}
}

func TestCollectorEntitlementMetricsPreserveChunks(t *testing.T) {
	fetcher := entitlementFetcher{snapshot: &cls.Snapshot{
		CollectedAt: time.Unix(1700000000, 0).UTC(),
		EntitlementFeatures: []cls.EntitlementFeatureSnapshot{
			{
				VirtualGroupID:     1,
				VirtualGroupName:   "DEFAULT_VG",
				EMSEntitlementID:   "ent-1",
				EMSProductKeyID:    "pk-72",
				FeatureName:        "NVIDIA RTX Virtual Workstation",
				FeatureVersion:     "1.0",
				ProductName:        "NVIDIA RTX Virtual Workstation",
				LicenseType:        "CONCURRENT",
				TotalQuantity:      72,
				InUseQuantity:      53,
				UnassignedQuantity: 0,
			},
			{
				VirtualGroupID:     1,
				VirtualGroupName:   "DEFAULT_VG",
				EMSEntitlementID:   "ent-1",
				EMSProductKeyID:    "pk-128",
				FeatureName:        "NVIDIA RTX Virtual Workstation",
				FeatureVersion:     "1.0",
				ProductName:        "NVIDIA RTX Virtual Workstation",
				LicenseType:        "CONCURRENT",
				TotalQuantity:      128,
				InUseQuantity:      0,
				UnassignedQuantity: 72,
			},
		},
	}}

	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(NewCollector(snapshot.NewService(fetcher, time.Minute), "org-1", time.Second))

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}

	got := make(map[string]float64)
	for _, family := range families {
		switch family.GetName() {
		case "nvidia_cls_entitlement_total_quantity", "nvidia_cls_entitlement_in_use_quantity", "nvidia_cls_entitlement_unassigned_quantity":
		default:
			continue
		}

		for _, metric := range family.GetMetric() {
			labels := make(map[string]string, len(metric.GetLabel()))
			for _, label := range metric.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			key := family.GetName() + "/" + labels["ems_entitlement_id"] + "/" + labels["ems_product_key_id"]
			got[key] = metric.GetGauge().GetValue()
		}
	}

	expected := map[string]float64{
		"nvidia_cls_entitlement_total_quantity/ent-1/pk-72":       72,
		"nvidia_cls_entitlement_total_quantity/ent-1/pk-128":      128,
		"nvidia_cls_entitlement_in_use_quantity/ent-1/pk-72":      53,
		"nvidia_cls_entitlement_in_use_quantity/ent-1/pk-128":     0,
		"nvidia_cls_entitlement_unassigned_quantity/ent-1/pk-72":  0,
		"nvidia_cls_entitlement_unassigned_quantity/ent-1/pk-128": 72,
	}
	if len(got) != len(expected) {
		t.Fatalf("expected %d entitlement series, got %d: %v", len(expected), len(got), got)
	}
	for key, want := range expected {
		if value, ok := got[key]; !ok || value != want {
			t.Errorf("metric %s: got %v, want %v", key, value, want)
		}
	}
}
