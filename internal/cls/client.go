package cls

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

const (
	defaultBaseURL           = "https://api.licensing.nvidia.com"
	defaultRequestTimeout    = 15 * time.Second
	defaultParallelFetches   = 8
	defaultUserAgent         = "nvidia-license-server-exporter/0.1"
	defaultContentTypeHeader = "application/json"
	maxPages                 = 500
	maxErrorBodyBytes        = 512
)

type Config struct {
	BaseURL           string
	APIKey            string
	OrgName           string
	ServiceInstanceID string
	HTTPClient        *http.Client
	ParallelFetches   int
}

type Client struct {
	baseURL           string
	apiKey            string
	orgName           string
	serviceInstanceID string
	httpClient        *http.Client
	parallelFetches   int
}

func NewClient(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("api key is required")
	}
	if strings.TrimSpace(cfg.OrgName) == "" {
		return nil, errors.New("org name is required")
	}

	baseURL := strings.TrimSpace(cfg.BaseURL)
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	baseURL = strings.TrimSuffix(baseURL, "/")

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultRequestTimeout}
	}

	parallelFetches := cfg.ParallelFetches
	if parallelFetches <= 0 {
		parallelFetches = defaultParallelFetches
	}

	return &Client{
		baseURL:           baseURL,
		apiKey:            strings.TrimSpace(cfg.APIKey),
		orgName:           strings.TrimSpace(cfg.OrgName),
		serviceInstanceID: strings.TrimSpace(cfg.ServiceInstanceID),
		httpClient:        httpClient,
		parallelFetches:   parallelFetches,
	}, nil
}

// Snapshot is one collection from the CLS API. NVIDIA now calls a license
// server an allocation; the Server* names are kept so the exported metric
// names and labels stay stable.
type Snapshot struct {
	CollectedAt               time.Time
	EntitlementFeatures       []EntitlementFeatureSnapshot
	ServerFeatureCapacity     []ServerFeatureCapacitySnapshot
	ServerInfo                []ServerInfoSnapshot
	ServerFeatureActiveLeases []ServerFeatureActiveLeaseSnapshot
}

type EntitlementFeatureSnapshot struct {
	VirtualGroupID     int
	VirtualGroupName   string
	EMSEntitlementID   string
	EMSProductKeyID    string
	FeatureName        string
	FeatureVersion     string
	ProductName        string
	LicenseType        string
	TotalQuantity      float64
	InUseQuantity      float64
	UnassignedQuantity float64
}

type ServerFeatureCapacitySnapshot struct {
	VirtualGroupID   int
	VirtualGroupName string
	ServerID         string
	ServerName       string
	FeatureName      string
	ProductName      string
	LicenseType      string
	TotalQuantity    float64
}

type ServerInfoSnapshot struct {
	VirtualGroupID   int
	VirtualGroupName string
	ServerID         string
	ServerName       string
	ServerStatus     string
	DeployedOn       string
	LeasingMode      string
}

type ServerFeatureActiveLeaseSnapshot struct {
	VirtualGroupID   int
	VirtualGroupName string
	ServerID         string
	ServerName       string
	FeatureName      string
	ProductName      string
	LicenseType      string
	ActiveLeases     float64
}

func (c *Client) FetchSnapshot(ctx context.Context) (*Snapshot, error) {
	virtualGroups, err := c.listVirtualGroups(ctx)
	if err != nil {
		return nil, err
	}

	snapshot := &Snapshot{
		CollectedAt:         time.Now().UTC(),
		EntitlementFeatures: extractEntitlementFeatureMetrics(virtualGroups),
	}

	var allocations []allocation
	var allocationsMu sync.Mutex
	allocationGroup, allocationCtx := errgroup.WithContext(ctx)
	allocationGroup.SetLimit(c.parallelFetches)
	for _, vg := range virtualGroups {
		allocationGroup.Go(func() error {
			listed, err := c.listAllocations(allocationCtx, vg)
			if err != nil {
				return fmt.Errorf("list allocations for virtual-group %d: %w", vg.ID, err)
			}
			allocationsMu.Lock()
			allocations = append(allocations, listed...)
			allocationsMu.Unlock()
			return nil
		})
	}
	if err := allocationGroup.Wait(); err != nil {
		return nil, err
	}

	var snapshotMu sync.Mutex
	leaseGroup, leaseCtx := errgroup.WithContext(ctx)
	leaseGroup.SetLimit(c.parallelFetches)
	for _, a := range allocations {
		leaseGroup.Go(func() error {
			leases, err := c.listAllocationLeases(leaseCtx, a)
			if err != nil {
				return fmt.Errorf("list active leases for allocation %s in virtual-group %d: %w", a.ID, a.VirtualGroupID, err)
			}
			snapshotMu.Lock()
			snapshot.ServerInfo = append(snapshot.ServerInfo, a.info())
			snapshot.ServerFeatureCapacity = append(snapshot.ServerFeatureCapacity, a.featureCapacity()...)
			snapshot.ServerFeatureActiveLeases = append(snapshot.ServerFeatureActiveLeases, a.featureActiveLeases(leases)...)
			snapshotMu.Unlock()
			return nil
		})
	}
	if err := leaseGroup.Wait(); err != nil {
		return nil, err
	}

	return snapshot, nil
}

func extractEntitlementFeatureMetrics(virtualGroups []virtualGroup) []EntitlementFeatureSnapshot {
	metrics := make([]EntitlementFeatureSnapshot, 0)
	for _, vg := range virtualGroups {
		for _, entitlement := range vg.Entitlements {
			for _, key := range entitlement.EntitlementProductKeys {
				for _, feature := range key.EntitlementFeatures {
					metrics = append(metrics, EntitlementFeatureSnapshot{
						VirtualGroupID:     vg.ID,
						VirtualGroupName:   vg.Name,
						EMSEntitlementID:   entitlement.EMSEntitlementID,
						EMSProductKeyID:    key.EMSProductKeyID,
						FeatureName:        feature.FeatureName,
						FeatureVersion:     feature.FeatureVersion,
						ProductName:        feature.ProductName,
						LicenseType:        feature.LicenseType,
						TotalQuantity:      feature.TotalQuantity,
						InUseQuantity:      feature.InUseQuantity,
						UnassignedQuantity: feature.UnassignedQuantity,
					})
				}
			}
		}
	}
	return metrics
}

func (c *Client) listVirtualGroups(ctx context.Context) ([]virtualGroup, error) {
	base := fmt.Sprintf("%s/v1/org/%s/virtual-groups", c.baseURL, url.PathEscape(c.orgName))
	seen := make(map[int]struct{})
	virtualGroups := make([]virtualGroup, 0)
	err := listPages(ctx, c, func(page int) string {
		query := url.Values{"page-number": {strconv.Itoa(page)}}
		return base + "?" + query.Encode()
	}, "", func(resp virtualGroupsResponse) (items, fresh int) {
		for _, vg := range resp.VirtualGroups {
			if _, ok := seen[vg.ID]; ok {
				continue
			}
			seen[vg.ID] = struct{}{}
			virtualGroups = append(virtualGroups, vg)
			fresh++
		}
		return len(resp.VirtualGroups), fresh
	})
	if err != nil {
		return nil, err
	}
	return virtualGroups, nil
}

// listAllocations walks the paginated allocations of a virtual group. The
// virtual group is filled in when the API omits it from an allocation.
func (c *Client) listAllocations(ctx context.Context, vg virtualGroup) ([]allocation, error) {
	base := fmt.Sprintf("%s/v1/org/%s/virtual-groups/%d/allocations", c.baseURL, url.PathEscape(c.orgName), vg.ID)
	seen := make(map[string]struct{})
	allocations := make([]allocation, 0)
	err := listPages(ctx, c, func(page int) string {
		query := url.Values{"page-number": {strconv.Itoa(page)}}
		return base + "?" + query.Encode()
	}, "", func(resp allocationsResponse) (items, fresh int) {
		for _, a := range resp.Allocations {
			if _, ok := seen[a.ID]; ok {
				continue
			}
			seen[a.ID] = struct{}{}
			if a.VirtualGroupID == 0 {
				a.VirtualGroupID = vg.ID
			}
			if a.VirtualGroupName == "" {
				a.VirtualGroupName = vg.Name
			}
			allocations = append(allocations, a)
			fresh++
		}
		return len(resp.Allocations), fresh
	})
	if err != nil {
		return nil, err
	}
	return allocations, nil
}

// listAllocationLeases walks the paginated active leases of one CLS allocation.
// A lease ID can repeat across pages, so each is returned once.
// A configured service instance fills in missing binding metadata; it does not
// override an allocation's type or deployment. NVIDIA omits the binding when a
// license file has been generated for the allocation, so the fallback does not
// apply to those.
func (c *Client) listAllocationLeases(ctx context.Context, a allocation) ([]lease, error) {
	if a.Type != "" && a.Type != "NVIDIA" {
		return nil, nil
	}
	if a.DeployedOn != "" && a.DeployedOn != "CLOUD" {
		return nil, nil
	}
	serviceInstanceID := strings.TrimSpace(a.ServiceInstanceID)
	if serviceInstanceID == "" && a.ArtifactType == "" {
		serviceInstanceID = c.serviceInstanceID
	}
	if serviceInstanceID == "" {
		if a.ArtifactType == "" {
			log.Printf("skipping active leases for allocation %s in virtual-group %d: no service instance is bound and NVIDIA_SERVICE_INSTANCE_ID is not set", a.ID, a.VirtualGroupID)
		}
		return nil, nil
	}
	base := fmt.Sprintf("%s/v1/org/%s/virtual-groups/%d/leases", c.baseURL, url.PathEscape(c.orgName), a.VirtualGroupID)
	seen := make(map[string]struct{})
	leases := make([]lease, 0)
	err := listPages(ctx, c, func(page int) string {
		query := url.Values{
			"license_server_ids": {a.ID},
			"page_number":        {strconv.Itoa(page)},
		}
		return base + "?" + query.Encode()
	}, serviceInstanceID, func(resp leasesResponse) (items, fresh int) {
		for _, client := range resp.Clients {
			for _, l := range client.Leases {
				items++
				if leaseID := strings.TrimSpace(l.LeaseID); leaseID != "" {
					if _, ok := seen[leaseID]; ok {
						continue
					}
					seen[leaseID] = struct{}{}
				}
				leases = append(leases, l)
				fresh++
			}
		}
		return items, fresh
	})
	if err != nil {
		return nil, err
	}
	return leases, nil
}

type pagedResponse interface {
	pagination() paginationInfo
}

// listPages visits every page of a listing, in order, until the API reports
// the last page. visit reports how many items the page held and how many of
// them it had not seen before. A page that only repeats earlier items means
// the API did not honor the page parameter; that and a listing that never ends
// are errors rather than a partial snapshot presented as complete.
func listPages[T pagedResponse](ctx context.Context, c *Client, pageURL func(page int) string, serviceInstanceID string, visit func(T) (items, fresh int)) error {
	for page := 0; ; page++ {
		if page >= maxPages {
			return fmt.Errorf("listing exceeded %d pages", maxPages)
		}
		var resp T
		if err := c.doJSON(ctx, http.MethodGet, pageURL(page), &resp, serviceInstanceID); err != nil {
			return err
		}
		if items, fresh := visit(resp); items > 0 && fresh == 0 {
			return fmt.Errorf("page %d of the listing only repeated earlier results", page)
		}
		if page+1 >= resp.pagination().TotalPages {
			return nil
		}
	}
}

func (c *Client) doJSON(ctx context.Context, method, endpoint string, out any, serviceInstanceID string) error {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("accept", defaultContentTypeHeader)
	req.Header.Set("user-agent", defaultUserAgent)
	if serviceInstanceID != "" {
		req.Header.Set("x-nv-service-instance-id", serviceInstanceID)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// NVIDIA echoes the submitted API key in unauthorized response bodies.
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("request %s failed with status %d", endpoint, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("request %s failed with status %d: %s", endpoint, resp.StatusCode, errorBody(resp.Body, c.apiKey))
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return err
	}
	return nil
}

// errorBody keeps a bounded, single-line excerpt of a failed response so the
// cause, such as a retired endpoint, is visible in the error. The API key is
// redacted before truncation so a key at the cut-off is not partially kept.
func errorBody(body io.Reader, apiKey string) string {
	raw, _ := io.ReadAll(io.LimitReader(body, int64(maxErrorBodyBytes+len(apiKey))))
	text := string(raw)
	if apiKey != "" {
		// Keep byte offsets stable so a partial key at the read limit cannot move into the excerpt.
		text = strings.ReplaceAll(text, apiKey, strings.Repeat("*", len(apiKey)))
	}
	if len(text) > maxErrorBodyBytes {
		text = text[:maxErrorBodyBytes]
	}
	excerpt := strings.Join(strings.Fields(text), " ")
	if excerpt == "" {
		return "empty response body"
	}
	return excerpt
}

func (c *Client) OrgName() string {
	return c.orgName
}

type virtualGroupsResponse struct {
	VirtualGroups  []virtualGroup `json:"virtualGroups"`
	PaginationInfo paginationInfo `json:"paginationInfo"`
}

func (r virtualGroupsResponse) pagination() paginationInfo {
	return r.PaginationInfo
}

type virtualGroup struct {
	ID           int                  `json:"id"`
	Name         string               `json:"name"`
	Entitlements []entitlementSummary `json:"entitlements"`
}

type entitlementSummary struct {
	EMSEntitlementID       string                  `json:"emsEntitlementId"`
	EntitlementProductKeys []entitlementProductKey `json:"entitlementProductKeys"`
}

type entitlementProductKey struct {
	EMSProductKeyID     string               `json:"emsProductKeyId"`
	EntitlementFeatures []entitlementFeature `json:"entitlementFeatures"`
}

type entitlementFeature struct {
	FeatureName        string  `json:"featureName"`
	FeatureVersion     string  `json:"featureVersion"`
	ProductName        string  `json:"productName"`
	LicenseType        string  `json:"licenseType"`
	TotalQuantity      float64 `json:"totalQuantity"`
	InUseQuantity      float64 `json:"inUseQuantity"`
	UnassignedQuantity float64 `json:"unassignedQuantity"`
}

type paginationInfo struct {
	Index      int `json:"index"`
	TotalPages int `json:"totalPages"`
}

type allocationsResponse struct {
	Allocations    []allocation   `json:"allocations"`
	PaginationInfo paginationInfo `json:"paginationInfo"`
}

func (r allocationsResponse) pagination() paginationInfo {
	return r.PaginationInfo
}

type allocation struct {
	ID                 string              `json:"id"`
	Type               string              `json:"type"`
	Name               string              `json:"name"`
	Status             string              `json:"status"`
	VirtualGroupID     int                 `json:"virtualGroupId"`
	VirtualGroupName   string              `json:"virtualGroupName"`
	DeployedOn         string              `json:"deployedOn"`
	LeasingMode        string              `json:"leasingMode"`
	ServiceInstanceID  string              `json:"serviceInstanceId"`
	ArtifactType       string              `json:"artifactType"`
	AllocationFeatures []allocationFeature `json:"allocationFeatures"`
}

type allocationFeature struct {
	ID            string  `json:"id"`
	FeatureName   string  `json:"featureName"`
	ProductName   string  `json:"productName"`
	LicenseType   string  `json:"licenseType"`
	TotalQuantity float64 `json:"totalQuantity"`
}

func (a allocation) info() ServerInfoSnapshot {
	return ServerInfoSnapshot{
		VirtualGroupID:   a.VirtualGroupID,
		VirtualGroupName: a.VirtualGroupName,
		ServerID:         a.ID,
		ServerName:       a.Name,
		ServerStatus:     a.Status,
		DeployedOn:       a.DeployedOn,
		LeasingMode:      a.LeasingMode,
	}
}

func (a allocation) featureCapacity() []ServerFeatureCapacitySnapshot {
	capacity := make([]ServerFeatureCapacitySnapshot, 0, len(a.AllocationFeatures))
	for _, feature := range a.AllocationFeatures {
		capacity = append(capacity, ServerFeatureCapacitySnapshot{
			VirtualGroupID:   a.VirtualGroupID,
			VirtualGroupName: a.VirtualGroupName,
			ServerID:         a.ID,
			ServerName:       a.Name,
			FeatureName:      feature.FeatureName,
			ProductName:      feature.ProductName,
			LicenseType:      feature.LicenseType,
			TotalQuantity:    feature.TotalQuantity,
		})
	}
	return capacity
}

type activeFeatureKey struct {
	featureName string
	productName string
	licenseType string
}

// featureActiveLeases counts leases per allocation feature.
func (a allocation) featureActiveLeases(leases []lease) []ServerFeatureActiveLeaseSnapshot {
	featureByID := make(map[string]allocationFeature, len(a.AllocationFeatures))
	for _, feature := range a.AllocationFeatures {
		featureByID[feature.ID] = feature
	}

	counts := make(map[activeFeatureKey]float64)
	order := make([]activeFeatureKey, 0)
	for _, l := range leases {
		feature := featureByID[l.LicenseAllotmentFeatureID]
		key := activeFeatureKey{
			featureName: firstNonEmptyNonBlank(feature.FeatureName, l.FeatureName, "unknown"),
			productName: firstNonEmptyNonBlank(feature.ProductName, "unknown"),
			licenseType: firstNonEmptyNonBlank(feature.LicenseType, "unknown"),
		}
		if _, ok := counts[key]; !ok {
			order = append(order, key)
		}
		counts[key]++
	}

	snapshots := make([]ServerFeatureActiveLeaseSnapshot, 0, len(order))
	for _, key := range order {
		snapshots = append(snapshots, ServerFeatureActiveLeaseSnapshot{
			VirtualGroupID:   a.VirtualGroupID,
			VirtualGroupName: a.VirtualGroupName,
			ServerID:         a.ID,
			ServerName:       a.Name,
			FeatureName:      key.featureName,
			ProductName:      key.productName,
			LicenseType:      key.licenseType,
			ActiveLeases:     counts[key],
		})
	}
	return snapshots
}

type leasesResponse struct {
	Clients        []leaseClient  `json:"clients"`
	PaginationInfo paginationInfo `json:"paginationInfo"`
}

func (r leasesResponse) pagination() paginationInfo {
	return r.PaginationInfo
}

type leaseClient struct {
	Leases []lease `json:"leases"`
}

type lease struct {
	LeaseID                   string `json:"leaseId"`
	FeatureName               string `json:"featureName"`
	LicenseAllotmentFeatureID string `json:"licenseAllotmentFeatureId"`
}

func firstNonEmptyNonBlank(values ...string) string {
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}
