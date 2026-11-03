package cls

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Fixtures follow the field names of the published NVIDIA Licensing Service
// OpenAPI document for the supported listing endpoints.
const (
	virtualGroupsBody = `{"virtualGroups":[{"id":42,"name":"DEFAULT_VG","entitlements":[{"emsEntitlementId":"ent-1","entitlementProductKeys":[{"emsProductKeyId":"pk-1","entitlementFeatures":[{"featureName":"NVIDIA RTX Virtual Workstation","featureVersion":"1.0","productName":"NVIDIA RTX Virtual Workstation","licenseType":"CONCURRENT_COUNTED_SINGLE","totalQuantity":200,"inUseQuantity":200,"unassignedQuantity":0}]}]}]}]}`

	allocationsPage0 = `{"allocations":[{"id":"alloc-1","type":"NVIDIA","name":"vgpu-license-server-0","status":"ENABLED","deployedOn":"CLOUD","leasingMode":"STANDARD","serviceInstanceId":"si-1","allocationFeatures":[{"id":"af-1","featureName":"NVIDIA RTX Virtual Workstation","productName":"NVIDIA RTX Virtual Workstation","licenseType":"CONCURRENT_COUNTED_SINGLE","totalQuantity":200}]}],"paginationInfo":{"index":0,"size":1,"totalResults":2,"totalPages":2}}`
	allocationsPage1 = `{"allocations":[{"id":"alloc-2","type":"NVIDIA","name":"not-deployed","status":"DISABLED","virtualGroupId":42,"virtualGroupName":"DEFAULT_VG","deployedOn":"NOT_DEPLOYED","allocationFeatures":[]}],"paginationInfo":{"index":1,"size":1,"totalResults":2,"totalPages":2}}`

	leasesPage0 = `{"clients":[{"hostname":"browser-a","leases":[{"leaseId":"lease-1","featureName":"NVIDIA RTX Virtual Workstation","licenseAllotmentFeatureId":"af-1"},{"leaseId":"lease-2","featureName":"NVIDIA RTX Virtual Workstation","licenseAllotmentFeatureId":"af-1"}]}],"paginationInfo":{"index":0,"size":2,"totalResults":4,"totalPages":2}}`
	leasesPage1 = `{"clients":[{"hostname":"browser-b","leases":[{"leaseId":"lease-2","featureName":"NVIDIA RTX Virtual Workstation","licenseAllotmentFeatureId":"af-1"},{"leaseId":"lease-3","featureName":"Unmapped Feature","licenseAllotmentFeatureId":"af-missing"}]}],"paginationInfo":{"index":1,"size":2,"totalResults":4,"totalPages":2}}`
)

type fakeCLS struct {
	mu       sync.Mutex
	requests []*http.Request
	// leases serves the active leases of one allocation per page, keyed by
	// allocation ID; allocations without an entry answer 404.
	leases map[string][]string
	// allocationPages overrides the default two allocation pages.
	allocationPages []string
}

func (f *fakeCLS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Clone(context.Background()))
	f.mu.Unlock()

	if r.Header.Get("x-api-key") != "test-key" {
		http.Error(w, `{"message":"Unauthorized"}`, http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/v1/org/lic-test/virtual-groups":
		_, _ = w.Write([]byte(virtualGroupsBody))
	case "/v1/org/lic-test/virtual-groups/42/allocations":
		pages := f.allocationPages
		if pages == nil {
			pages = []string{allocationsPage0, allocationsPage1}
		}
		_, _ = w.Write([]byte(pages[pageIndex(r.URL.Query().Get("page-number"), len(pages))]))
	case "/v1/org/lic-test/virtual-groups/42/leases":
		pages, ok := f.leases[r.URL.Query().Get("license_server_ids")]
		if !ok || r.Header.Get("x-nv-service-instance-id") == "" {
			http.Error(w, `{"message":"Service instance or license server was not found in the system."}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(pages[pageIndex(r.URL.Query().Get("page_number"), len(pages))]))
	default:
		http.Error(w, `{"message":"This API is no longer supported."}`, http.StatusBadRequest)
	}
}

func pageIndex(raw string, pages int) int {
	var page int
	_, _ = fmt.Sscanf(raw, "%d", &page)
	if page >= pages {
		return pages - 1
	}
	return page
}

func (f *fakeCLS) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	paths := make([]string, 0, len(f.requests))
	for _, r := range f.requests {
		paths = append(paths, r.URL.RequestURI())
	}
	return paths
}

func newTestClient(t *testing.T, fake *fakeCLS, cfg Config) *Client {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	cfg.BaseURL = server.URL
	cfg.HTTPClient = server.Client()
	if cfg.APIKey == "" {
		cfg.APIKey = "test-key"
	}
	if cfg.OrgName == "" {
		cfg.OrgName = "lic-test"
	}
	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	return client
}

func TestFetchSnapshotUsesSupportedEndpoints(t *testing.T) {
	fake := &fakeCLS{leases: map[string][]string{"alloc-1": {leasesPage0, leasesPage1}}}
	client := newTestClient(t, fake, Config{ServiceInstanceID: "si-default"})

	snap, err := client.FetchSnapshot(context.Background())
	if err != nil {
		t.Fatalf("fetch snapshot: %v", err)
	}

	for _, path := range fake.paths() {
		for _, retired := range []string{"license-pools", "license-servers", "leases/all"} {
			if strings.Contains(path, retired) {
				t.Fatalf("request to retired endpoint: %s", path)
			}
		}
	}
	wantPaths := []string{
		"/v1/org/lic-test/virtual-groups",
		"/v1/org/lic-test/virtual-groups/42/allocations?page-number=0",
		"/v1/org/lic-test/virtual-groups/42/allocations?page-number=1",
		"/v1/org/lic-test/virtual-groups/42/leases?license_server_ids=alloc-1&page_number=0",
		"/v1/org/lic-test/virtual-groups/42/leases?license_server_ids=alloc-1&page_number=1",
	}
	if got := fake.paths(); strings.Join(got, "\n") != strings.Join(wantPaths, "\n") {
		t.Fatalf("requests = %v, want %v", got, wantPaths)
	}
	for _, r := range fake.requests {
		if r.URL.Path == "/v1/org/lic-test/virtual-groups/42/leases" && r.Header.Get("x-nv-service-instance-id") != "si-1" {
			t.Fatalf("lease request service instance = %q, want si-1", r.Header.Get("x-nv-service-instance-id"))
		}
	}

	if len(snap.EntitlementFeatures) != 1 || snap.EntitlementFeatures[0].TotalQuantity != 200 {
		t.Fatalf("unexpected entitlement features: %+v", snap.EntitlementFeatures)
	}

	info := make(map[string]ServerInfoSnapshot, len(snap.ServerInfo))
	for _, item := range snap.ServerInfo {
		info[item.ServerID] = item
	}
	wantInfo := map[string]ServerInfoSnapshot{
		"alloc-1": {VirtualGroupID: 42, VirtualGroupName: "DEFAULT_VG", ServerID: "alloc-1", ServerName: "vgpu-license-server-0", ServerStatus: "ENABLED", DeployedOn: "CLOUD", LeasingMode: "STANDARD"},
		"alloc-2": {VirtualGroupID: 42, VirtualGroupName: "DEFAULT_VG", ServerID: "alloc-2", ServerName: "not-deployed", ServerStatus: "DISABLED", DeployedOn: "NOT_DEPLOYED"},
	}
	if len(info) != len(wantInfo) {
		t.Fatalf("server info = %+v, want %+v", info, wantInfo)
	}
	for id, want := range wantInfo {
		if info[id] != want {
			t.Fatalf("server info %s = %+v, want %+v", id, info[id], want)
		}
	}

	if len(snap.ServerFeatureCapacity) != 1 {
		t.Fatalf("server feature capacity = %+v, want one feature", snap.ServerFeatureCapacity)
	}
	if got := snap.ServerFeatureCapacity[0]; got.ServerID != "alloc-1" || got.VirtualGroupID != 42 || got.FeatureName != "NVIDIA RTX Virtual Workstation" || got.TotalQuantity != 200 {
		t.Fatalf("unexpected server feature capacity: %+v", got)
	}

	active := make(map[string]ServerFeatureActiveLeaseSnapshot, len(snap.ServerFeatureActiveLeases))
	for _, item := range snap.ServerFeatureActiveLeases {
		active[item.FeatureName] = item
	}
	wantActive := map[string]ServerFeatureActiveLeaseSnapshot{
		"NVIDIA RTX Virtual Workstation": {VirtualGroupID: 42, VirtualGroupName: "DEFAULT_VG", ServerID: "alloc-1", ServerName: "vgpu-license-server-0", FeatureName: "NVIDIA RTX Virtual Workstation", ProductName: "NVIDIA RTX Virtual Workstation", LicenseType: "CONCURRENT_COUNTED_SINGLE", ActiveLeases: 2},
		"Unmapped Feature":               {VirtualGroupID: 42, VirtualGroupName: "DEFAULT_VG", ServerID: "alloc-1", ServerName: "vgpu-license-server-0", FeatureName: "Unmapped Feature", ProductName: "unknown", LicenseType: "unknown", ActiveLeases: 1},
	}
	if len(active) != len(wantActive) {
		t.Fatalf("active leases = %+v, want %+v", active, wantActive)
	}
	for name, want := range wantActive {
		if active[name] != want {
			t.Fatalf("active leases %s = %+v, want %+v", name, active[name], want)
		}
	}
}

func TestFetchSnapshotUsesConfiguredServiceInstanceFallback(t *testing.T) {
	fake := &fakeCLS{
		allocationPages: []string{`{"allocations":[{"id":"alloc-1","type":"NVIDIA","deployedOn":"CLOUD","allocationFeatures":[]}],"paginationInfo":{"index":0,"totalPages":1}}`},
		leases:          map[string][]string{"alloc-1": {`{"clients":[],"paginationInfo":{"index":0,"size":0,"totalResults":0,"totalPages":0}}`}},
	}
	client := newTestClient(t, fake, Config{ServiceInstanceID: "si-default"})

	if _, err := client.FetchSnapshot(context.Background()); err != nil {
		t.Fatalf("fetch snapshot: %v", err)
	}
	var leaseRequests int
	for _, r := range fake.requests {
		if r.URL.Path != "/v1/org/lic-test/virtual-groups/42/leases" {
			continue
		}
		leaseRequests++
		if got := r.Header.Get("x-nv-service-instance-id"); got != "si-default" {
			t.Fatalf("lease request service instance = %q, want si-default", got)
		}
	}
	if leaseRequests != 1 {
		t.Fatalf("lease requests = %d, want 1", leaseRequests)
	}
}

func TestFetchSnapshotSkipsNonCLSLeases(t *testing.T) {
	for _, tc := range []struct {
		name              string
		allocationType    string
		deployedOn        string
		serviceInstanceID string
	}{
		{name: "undeployed", allocationType: "NVIDIA", deployedOn: "NOT_DEPLOYED"},
		{name: "not applicable", allocationType: "NVIDIA", deployedOn: "NOT_APPLICABLE"},
		{name: "on premises", allocationType: "NVIDIA", deployedOn: "ON_PREM", serviceInstanceID: "si-dls"},
		{name: "mellanox", allocationType: "MELLANOX"},
		{name: "bright", allocationType: "BRIGHT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeCLS{allocationPages: []string{fmt.Sprintf(`{"allocations":[{"id":"alloc-2","type":%q,"deployedOn":%q,"serviceInstanceId":%q,"allocationFeatures":[{"id":"af-1","featureName":"Feature","productName":"Product","licenseType":"CONCURRENT_COUNTED_SINGLE","totalQuantity":10}]}],"paginationInfo":{"totalPages":1}}`, tc.allocationType, tc.deployedOn, tc.serviceInstanceID)}}
			client := newTestClient(t, fake, Config{ServiceInstanceID: "si-default"})

			snap, err := client.FetchSnapshot(context.Background())
			if err != nil {
				t.Fatalf("fetch snapshot: %v", err)
			}
			if len(snap.ServerInfo) != 1 || len(snap.ServerFeatureCapacity) != 1 || snap.ServerFeatureCapacity[0].TotalQuantity != 10 {
				t.Fatalf("expected allocation metadata and capacity to be preserved, got %+v", snap)
			}
			if len(snap.ServerFeatureActiveLeases) != 0 {
				t.Fatalf("unexpected active leases: %+v", snap.ServerFeatureActiveLeases)
			}
			if paths := fake.paths(); len(paths) != 2 {
				t.Fatalf("expected only virtual-group and allocation requests, got %v", paths)
			}
		})
	}
}

func TestFetchSnapshotErrorIncludesResponseBody(t *testing.T) {
	fake := &fakeCLS{allocationPages: []string{`{"allocations":[{"id":"alloc-1","serviceInstanceId":"si-1"}],"paginationInfo":{"totalPages":1}}`}}
	client := newTestClient(t, fake, Config{})

	snap, err := client.FetchSnapshot(context.Background())
	if snap != nil || err == nil {
		t.Fatalf("expected failed snapshot, got snapshot=%v err=%v", snap, err)
	}
	for _, want := range []string{"allocation alloc-1", "failed with status 404", "not found in the system"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

func TestFetchSnapshotRejectsRunawayListing(t *testing.T) {
	fake := &fakeCLS{allocationPages: []string{fmt.Sprintf(`{"allocations":[],"paginationInfo":{"index":0,"totalPages":%d}}`, maxPages+1)}}
	client := newTestClient(t, fake, Config{})

	_, err := client.FetchSnapshot(context.Background())
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("exceeded %d pages", maxPages)) {
		t.Fatalf("expected page cap error, got %v", err)
	}
	if got := len(fake.paths()); got != maxPages+1 {
		t.Fatalf("requests = %d, want %d", got, maxPages+1)
	}
}

func TestErrorBody(t *testing.T) {
	if got := errorBody(strings.NewReader("")); got != "empty response body" {
		t.Fatalf("empty body = %q", got)
	}
	if got := errorBody(strings.NewReader("{\n  \"message\":\t\"This API is no longer supported.\"\n}\n")); got != `{ "message": "This API is no longer supported." }` {
		t.Fatalf("collapsed body = %q", got)
	}
	if got := errorBody(strings.NewReader(strings.Repeat("x", 2*maxErrorBodyBytes))); len(got) != maxErrorBodyBytes {
		t.Fatalf("bounded body length = %d, want %d", len(got), maxErrorBodyBytes)
	}
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
