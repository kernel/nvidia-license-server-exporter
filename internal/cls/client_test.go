package cls

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"testing"
)

func TestActiveLeasesResponseClientsPlain(t *testing.T) {
	resp := activeLeasesResponse{
		Clients: []activeLeaseClient{
			{Leases: []activeLease{{LeaseID: "lease-1"}}},
		},
	}

	clients, err := resp.clients()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(clients) != 1 || clients[0].Leases[0].LeaseID != "lease-1" {
		t.Fatalf("unexpected clients: %+v", clients)
	}
}

func TestActiveLeasesResponseClientsCompressed(t *testing.T) {
	payload := `{"clients":[{"leases":[{"leaseId":"lease-1"}]},{"leases":[{"leaseId":"lease-2"}]}]}`

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte(payload)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	resp := activeLeasesResponse{
		CompressedClients: base64.StdEncoding.EncodeToString(buf.Bytes()),
	}

	clients, err := resp.clients()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(clients) != 2 || clients[0].Leases[0].LeaseID != "lease-1" || clients[1].Leases[0].LeaseID != "lease-2" {
		t.Fatalf("unexpected clients: %+v", clients)
	}
}

func TestActiveLeasesResponseClientsBadBase64(t *testing.T) {
	resp := activeLeasesResponse{CompressedClients: "not-base64!!"}
	if _, err := resp.clients(); err == nil {
		t.Fatal("expected error for invalid base64")
	}
}

func TestExtractEntitlementFeatureMetricsIDs(t *testing.T) {
	groups := []virtualGroup{
		{
			ID:   1,
			Name: "DEFAULT_VG",
			Entitlements: []entitlementSummary{
				{
					EmsEntitlementID: "ent-1",
					EntitlementProductKeys: []entitlementProductKey{
						{
							EmsProductKeyID: "pk-72",
							EntitlementFeatures: []entitlementFeature{
								{FeatureName: "NVIDIA RTX Virtual Workstation", TotalQuantity: 72, InUseQuantity: 53, UnassignedQuantity: 0},
							},
						},
						{
							EmsProductKeyID: "pk-128",
							EntitlementFeatures: []entitlementFeature{
								{FeatureName: "NVIDIA RTX Virtual Workstation", TotalQuantity: 128, InUseQuantity: 0, UnassignedQuantity: 72},
							},
						},
					},
				},
			},
		},
	}

	metrics := extractEntitlementFeatureMetrics(groups)
	if len(metrics) != 2 {
		t.Fatalf("expected 2 entitlement features, got %d", len(metrics))
	}
	if metrics[0].EmsProductKeyID == metrics[1].EmsProductKeyID {
		t.Fatalf("expected distinct product key ids, got %q for both", metrics[0].EmsProductKeyID)
	}
	if metrics[0].EmsEntitlementID != "ent-1" || metrics[1].EmsEntitlementID != "ent-1" {
		t.Fatalf("unexpected entitlement ids: %+v", metrics)
	}
}
