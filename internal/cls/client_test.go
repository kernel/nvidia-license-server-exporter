package cls

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestActiveLeasesResponseClientsCompressed(t *testing.T) {
	resp := activeLeasesResponse{
		CompressedClients: gzipBase64(t, `{"clients":[{"leases":[{"leaseId":"lease-1"}]},{"leases":[{"leaseId":"lease-2"}]}]}`),
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

func TestListActiveLeasesHTTPContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method: got %s, want GET", r.Method)
		}
		if r.URL.Path != "/v1/org/lic-test/virtual-groups/42/leases/all" {
			t.Errorf("path: got %s", r.URL.Path)
		}
		if got := r.Header.Get("x-nv-service-instance-id"); got != "instance-1" {
			t.Errorf("x-nv-service-instance-id: got %q, want %q", got, "instance-1")
		}
		if got := r.Header.Get("x-api-key"); got != "test-key" {
			t.Errorf("x-api-key: got %q, want %q", got, "test-key")
		}
		resp := activeLeasesResponse{Clients: []activeLeaseClient{{Leases: []activeLease{{LeaseID: "lease-1"}}}}}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{
		BaseURL:    server.URL,
		APIKey:     "test-key",
		OrgName:    "lic-test",
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	clients, err := client.listActiveLeases(context.Background(), 42, "instance-1")
	if err != nil {
		t.Fatalf("list active leases: %v", err)
	}
	if len(clients) != 1 || len(clients[0].Leases) != 1 || clients[0].Leases[0].LeaseID != "lease-1" {
		t.Fatalf("unexpected clients: %+v", clients)
	}
}

func gzipBase64(t *testing.T, payload string) string {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte(payload)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestExtractEntitlementFeatureMetricsIDs(t *testing.T) {
	groups := []virtualGroup{
		{
			ID:   1,
			Name: "DEFAULT_VG",
			Entitlements: []entitlementSummary{
				{
					EMSEntitlementID: "ent-1",
					EntitlementProductKeys: []entitlementProductKey{
						{
							EMSProductKeyID: "pk-72",
							EntitlementFeatures: []entitlementFeature{
								{FeatureName: "NVIDIA RTX Virtual Workstation", TotalQuantity: 72, InUseQuantity: 53, UnassignedQuantity: 0},
							},
						},
						{
							EMSProductKeyID: "pk-128",
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
	if metrics[0].EMSProductKeyID == metrics[1].EMSProductKeyID {
		t.Fatalf("expected distinct product key ids, got %q for both", metrics[0].EMSProductKeyID)
	}
	if metrics[0].EMSEntitlementID != "ent-1" || metrics[1].EMSEntitlementID != "ent-1" {
		t.Fatalf("unexpected entitlement ids: %+v", metrics)
	}
}
