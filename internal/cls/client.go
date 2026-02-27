package cls

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
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

type Snapshot struct {
	CollectedAt               time.Time
	EntitlementFeatures       []EntitlementFeatureSnapshot
	ServerFeatureCapacity     []ServerFeatureCapacitySnapshot
	ServerUsage               []ServerUsageSnapshot
	ServerActiveLeases        []ServerActiveLeaseSnapshot
	ServerFeatureActiveLeases []ServerFeatureActiveLeaseSnapshot
	ActiveLeaseTotal          float64
	PoolUsage                 []PoolUsageSnapshot
}

type EntitlementFeatureSnapshot struct {
	VirtualGroupID   int
	VirtualGroupName string
	FeatureName      string
	FeatureVersion   string
	ProductName      string
	LicenseType      string
	TotalQuantity    float64
	InUseQuantity    float64
	Unassigned       float64
}

type ServerFeatureCapacitySnapshot struct {
	VirtualGroupID   int
	VirtualGroupName string
	ServerID         string
	ServerName       string
	ServerStatus     string
	DeployedOn       string
	LeasingMode      string
	FeatureName      string
	ProductName      string
	LicenseType      string
	TotalQuantity    float64
}

type ServerUsageSnapshot struct {
	VirtualGroupID   int
	VirtualGroupName string
	ServerID         string
	ServerName       string
	ServerStatus     string
	DeployedOn       string
	LeasingMode      string
	Allocated        float64
	InUse            float64
	Available        float64
}

type ServerActiveLeaseSnapshot struct {
	VirtualGroupID   int
	VirtualGroupName string
	ServerID         string
	ServerName       string
	ActiveLeases     float64
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

type PoolUsageSnapshot struct {
	VirtualGroupID   int
	VirtualGroupName string
	ServerID         string
	ServerName       string
	PoolID           string
	PoolName         string
	FeatureName      string
	ProductName      string
	LicenseType      string
	Allocated        float64
	InUse            float64
	Available        float64
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

	serversByVG := make(map[int][]licenseServer, len(virtualGroups))
	serverGroup, groupCtx := errgroup.WithContext(ctx)
	serverGroup.SetLimit(c.parallelFetches)

	var serverMu sync.Mutex
	for _, vg := range virtualGroups {
		vg := vg
		serverGroup.Go(func() error {
			servers, listErr := c.listLicenseServers(groupCtx, vg.ID)
			if listErr != nil {
				return fmt.Errorf("list license servers for virtual-group %d: %w", vg.ID, listErr)
			}
			for i := range servers {
				if servers[i].VirtualGroupID == 0 {
					servers[i].VirtualGroupID = vg.ID
				}
				if servers[i].VirtualGroupName == "" {
					servers[i].VirtualGroupName = vg.Name
				}
			}
			serverMu.Lock()
			serversByVG[vg.ID] = servers
			serverMu.Unlock()
			return nil
		})
	}
	if err := serverGroup.Wait(); err != nil {
		return nil, err
	}

	activeByServer, serverActiveLeases, serverFeatureActiveLeases, activeLeaseTotal, err := c.fetchActiveLeaseUsage(ctx, serversByVG)
	if err != nil {
		return nil, err
	}
	snapshot.ActiveLeaseTotal = activeLeaseTotal
	snapshot.ServerActiveLeases = serverActiveLeases
	snapshot.ServerFeatureActiveLeases = serverFeatureActiveLeases

	poolGroup, poolCtx := errgroup.WithContext(ctx)
	poolGroup.SetLimit(c.parallelFetches)

	var snapshotMu sync.Mutex
	for _, vg := range virtualGroups {
		vg := vg
		servers := serversByVG[vg.ID]
		for _, server := range servers {
			server := server
			poolGroup.Go(func() error {
				pools, listErr := c.listLicensePools(poolCtx, vg.ID, server.ID)
				if listErr != nil {
					return fmt.Errorf("list license pools for server %s in virtual-group %d: %w", server.ID, vg.ID, listErr)
				}

				featureByID := make(map[string]licenseServerFeature, len(server.LicenseServerFeatures))
				serverFeatureCapacity := make([]ServerFeatureCapacitySnapshot, 0, len(server.LicenseServerFeatures))
				for _, feature := range server.LicenseServerFeatures {
					featureByID[feature.ID] = feature
					serverFeatureCapacity = append(serverFeatureCapacity, ServerFeatureCapacitySnapshot{
						VirtualGroupID:   server.VirtualGroupID,
						VirtualGroupName: server.VirtualGroupName,
						ServerID:         server.ID,
						ServerName:       server.Name,
						ServerStatus:     server.Status,
						DeployedOn:       server.DeployedOn,
						LeasingMode:      server.LeasingMode,
						FeatureName:      feature.FeatureName,
						ProductName:      feature.ProductName,
						LicenseType:      feature.LicenseType,
						TotalQuantity:    feature.TotalQuantity,
					})
				}

				poolUsage := make([]PoolUsageSnapshot, 0)
				var serverAllocated float64
				var serverInUse float64

				for _, pool := range pools {
					for _, feature := range pool.LicensePoolFeatures {
						serverFeature := featureByID[feature.LicenseServerFeatureID]
						allocated := feature.TotalAllotment
						inUse := feature.InUse
						available := allocated - inUse
						if available < 0 {
							available = 0
						}

						serverAllocated += allocated
						serverInUse += inUse
						poolUsage = append(poolUsage, PoolUsageSnapshot{
							VirtualGroupID:   server.VirtualGroupID,
							VirtualGroupName: server.VirtualGroupName,
							ServerID:         server.ID,
							ServerName:       server.Name,
							PoolID:           pool.ID,
							PoolName:         pool.Name,
							FeatureName:      serverFeature.FeatureName,
							ProductName:      serverFeature.ProductName,
							LicenseType:      serverFeature.LicenseType,
							Allocated:        allocated,
							InUse:            inUse,
							Available:        available,
						})
					}
				}

				serverUsage := ServerUsageSnapshot{
					VirtualGroupID:   server.VirtualGroupID,
					VirtualGroupName: server.VirtualGroupName,
					ServerID:         server.ID,
					ServerName:       server.Name,
					ServerStatus:     server.Status,
					DeployedOn:       server.DeployedOn,
					LeasingMode:      server.LeasingMode,
					Allocated:        serverAllocated,
					InUse:            serverInUse,
					Available:        maxFloat64(0, serverAllocated-serverInUse),
				}
				if activeLeaseCount, ok := activeByServer[server.ID]; ok {
					serverUsage.InUse = activeLeaseCount
					serverUsage.Available = maxFloat64(0, serverAllocated-activeLeaseCount)
				}

				snapshotMu.Lock()
				snapshot.PoolUsage = append(snapshot.PoolUsage, poolUsage...)
				snapshot.ServerUsage = append(snapshot.ServerUsage, serverUsage)
				snapshot.ServerFeatureCapacity = append(snapshot.ServerFeatureCapacity, serverFeatureCapacity...)
				snapshotMu.Unlock()
				return nil
			})
		}
	}
	if err := poolGroup.Wait(); err != nil {
		return nil, err
	}

	return snapshot, nil
}

type activeFeatureKey struct {
	virtualGroupID   int
	virtualGroupName string
	serverID         string
	serverName       string
	featureName      string
	productName      string
	licenseType      string
}

func (c *Client) fetchActiveLeaseUsage(ctx context.Context, serversByVG map[int][]licenseServer) (map[string]float64, []ServerActiveLeaseSnapshot, []ServerFeatureActiveLeaseSnapshot, float64, error) {
	serverTotals := make(map[string]float64)
	featureTotals := make(map[activeFeatureKey]float64)
	seenLeaseIDs := make(map[string]struct{})

	activeGroup, activeCtx := errgroup.WithContext(ctx)
	activeGroup.SetLimit(c.parallelFetches)

	var total float64
	var mu sync.Mutex

	for virtualGroupID, servers := range serversByVG {
		if len(servers) == 0 {
			continue
		}

		virtualGroupID := virtualGroupID
		virtualGroupName := servers[0].VirtualGroupName
		serverByID := make(map[string]licenseServer, len(servers))
		featureByAllotmentID := make(map[string]licenseServerFeature)
		serviceInstanceIDs := make(map[string]struct{})

		for _, server := range servers {
			serverByID[server.ID] = server
			for _, feature := range server.LicenseServerFeatures {
				featureByAllotmentID[feature.ID] = feature
			}
			if strings.TrimSpace(server.ServiceInstanceID) != "" {
				serviceInstanceIDs[server.ServiceInstanceID] = struct{}{}
			}
		}

		for serviceInstanceID := range serviceInstanceIDs {
			serviceInstanceID := serviceInstanceID
			activeGroup.Go(func() error {
				clients, err := c.listActiveLeases(activeCtx, virtualGroupID, serviceInstanceID)
				if err != nil {
					return fmt.Errorf("list active leases for virtual-group %d service-instance %s: %w", virtualGroupID, serviceInstanceID, err)
				}

				for _, client := range clients {
					serverID := strings.TrimSpace(client.AdditionalProperties.LicenseServerID)
					if serverID == "" && len(serverByID) == 1 {
						for onlyID := range serverByID {
							serverID = onlyID
							break
						}
					}
					if serverID == "" {
						continue
					}

					server := serverByID[serverID]
					serverName := firstNonEmptyNonBlank(client.AdditionalProperties.LicenseServerName, server.Name)
					if serverName == "" {
						serverName = "unknown"
					}

					for _, lease := range client.Leases {
						leaseCount := lease.LeaseCount
						if leaseCount <= 0 {
							leaseCount = 1
						}

						feature := featureByAllotmentID[lease.LicenseAllotmentFeatureID]
						featureName := firstNonEmptyNonBlank(lease.FeatureName, feature.FeatureName)
						productName := firstNonEmptyNonBlank(feature.ProductName, "unknown")
						licenseType := firstNonEmptyNonBlank(feature.LicenseType, "unknown")

						key := activeFeatureKey{
							virtualGroupID:   virtualGroupID,
							virtualGroupName: firstNonEmptyNonBlank(server.VirtualGroupName, virtualGroupName),
							serverID:         serverID,
							serverName:       serverName,
							featureName:      firstNonEmptyNonBlank(featureName, "unknown"),
							productName:      productName,
							licenseType:      licenseType,
						}

						leaseID := strings.TrimSpace(lease.LeaseID)
						mu.Lock()
						if leaseID != "" {
							if _, exists := seenLeaseIDs[leaseID]; exists {
								mu.Unlock()
								continue
							}
							seenLeaseIDs[leaseID] = struct{}{}
						}
						serverTotals[serverID] += leaseCount
						featureTotals[key] += leaseCount
						total += leaseCount
						mu.Unlock()
					}
				}
				return nil
			})
		}
	}

	if err := activeGroup.Wait(); err != nil {
		return nil, nil, nil, 0, err
	}

	serverSnapshots := make([]ServerActiveLeaseSnapshot, 0, len(serverTotals))
	for virtualGroupID, servers := range serversByVG {
		for _, server := range servers {
			count, ok := serverTotals[server.ID]
			if !ok {
				continue
			}
			serverSnapshots = append(serverSnapshots, ServerActiveLeaseSnapshot{
				VirtualGroupID:   virtualGroupID,
				VirtualGroupName: server.VirtualGroupName,
				ServerID:         server.ID,
				ServerName:       server.Name,
				ActiveLeases:     count,
			})
		}
	}

	featureSnapshots := make([]ServerFeatureActiveLeaseSnapshot, 0, len(featureTotals))
	for key, count := range featureTotals {
		featureSnapshots = append(featureSnapshots, ServerFeatureActiveLeaseSnapshot{
			VirtualGroupID:   key.virtualGroupID,
			VirtualGroupName: key.virtualGroupName,
			ServerID:         key.serverID,
			ServerName:       key.serverName,
			FeatureName:      key.featureName,
			ProductName:      key.productName,
			LicenseType:      key.licenseType,
			ActiveLeases:     count,
		})
	}

	return serverTotals, serverSnapshots, featureSnapshots, total, nil
}

func extractEntitlementFeatureMetrics(virtualGroups []virtualGroup) []EntitlementFeatureSnapshot {
	metrics := make([]EntitlementFeatureSnapshot, 0)
	for _, vg := range virtualGroups {
		for _, entitlement := range vg.Entitlements {
			for _, key := range entitlement.EntitlementProductKeys {
				for _, feature := range key.EntitlementFeatures {
					metrics = append(metrics, EntitlementFeatureSnapshot{
						VirtualGroupID:   vg.ID,
						VirtualGroupName: vg.Name,
						FeatureName:      feature.FeatureName,
						FeatureVersion:   feature.FeatureVersion,
						ProductName:      feature.ProductName,
						LicenseType:      feature.LicenseType,
						TotalQuantity:    feature.TotalQuantity,
						InUseQuantity:    feature.InUseQuantity,
						Unassigned:       feature.UnassignedQuantity,
					})
				}
			}
		}
	}
	return metrics
}

func (c *Client) listVirtualGroups(ctx context.Context) ([]virtualGroup, error) {
	endpoint := fmt.Sprintf("%s/v1/org/%s/virtual-groups", c.baseURL, url.PathEscape(c.orgName))
	var resp virtualGroupsResponse
	if err := c.doJSON(ctx, http.MethodGet, endpoint, &resp, ""); err != nil {
		return nil, err
	}
	return resp.VirtualGroups, nil
}

func (c *Client) listLicenseServers(ctx context.Context, virtualGroupID int) ([]licenseServer, error) {
	endpoint := fmt.Sprintf(
		"%s/v1/org/%s/virtual-groups/%d/license-servers",
		c.baseURL,
		url.PathEscape(c.orgName),
		virtualGroupID,
	)
	var resp licenseServersResponse
	if err := c.doJSON(ctx, http.MethodGet, endpoint, &resp, ""); err != nil {
		return nil, err
	}
	return resp.LicenseServers, nil
}

func (c *Client) listLicensePools(ctx context.Context, virtualGroupID int, serverID string) ([]licensePool, error) {
	endpoint := fmt.Sprintf(
		"%s/v1/org/%s/virtual-groups/%d/license-servers/%s/license-pools",
		c.baseURL,
		url.PathEscape(c.orgName),
		virtualGroupID,
		url.PathEscape(serverID),
	)
	var resp licensePoolsResponse
	if err := c.doJSON(ctx, http.MethodGet, endpoint, &resp, ""); err != nil {
		return nil, err
	}
	return resp.LicensePools, nil
}

func (c *Client) listActiveLeases(ctx context.Context, virtualGroupID int, serviceInstanceID string) ([]activeLeaseClient, error) {
	endpoint := fmt.Sprintf(
		"%s/v1/org/%s/virtual-groups/%d/leases",
		c.baseURL,
		url.PathEscape(c.orgName),
		virtualGroupID,
	)
	var resp activeLeasesResponse
	if err := c.doJSON(ctx, http.MethodGet, endpoint, &resp, serviceInstanceID); err != nil {
		return nil, err
	}
	return resp.Clients, nil
}

func (c *Client) doJSON(ctx context.Context, method, endpoint string, out any, serviceInstanceID string) error {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("accept", defaultContentTypeHeader)
	req.Header.Set("user-agent", defaultUserAgent)
	headerServiceInstanceID := strings.TrimSpace(serviceInstanceID)
	if headerServiceInstanceID == "" {
		headerServiceInstanceID = c.serviceInstanceID
	}
	if headerServiceInstanceID != "" {
		req.Header.Set("x-nv-service-instance-id", headerServiceInstanceID)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("request %s failed with status %d", endpoint, resp.StatusCode)
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return err
	}
	return nil
}

func (c *Client) OrgName() string {
	return c.orgName
}

func maxFloat64(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

type virtualGroupsResponse struct {
	VirtualGroups []virtualGroup `json:"virtualGroups"`
}

type virtualGroup struct {
	ID           int                  `json:"id"`
	Name         string               `json:"name"`
	Entitlements []entitlementSummary `json:"entitlements"`
}

type entitlementSummary struct {
	EntitlementProductKeys []entitlementProductKey `json:"entitlementProductKeys"`
}

type entitlementProductKey struct {
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

type licenseServersResponse struct {
	LicenseServers []licenseServer `json:"licenseServers"`
}

type licenseServer struct {
	ID                    string                 `json:"id"`
	Name                  string                 `json:"name"`
	Status                string                 `json:"status"`
	VirtualGroupID        int                    `json:"virtualGroupId"`
	VirtualGroupName      string                 `json:"virtualGroupName"`
	DeployedOn            string                 `json:"deployedOn"`
	LeasingMode           string                 `json:"leasingMode"`
	ServiceInstanceID     string                 `json:"serviceInstanceId"`
	LicenseServerFeatures []licenseServerFeature `json:"licenseServerFeatures"`
}

type licenseServerFeature struct {
	ID            string  `json:"id"`
	FeatureName   string  `json:"featureName"`
	ProductName   string  `json:"productName"`
	LicenseType   string  `json:"licenseType"`
	TotalQuantity float64 `json:"totalQuantity"`
}

type licensePoolsResponse struct {
	LicensePools []licensePool `json:"licensePools"`
}

type licensePool struct {
	ID                  string               `json:"id"`
	Name                string               `json:"name"`
	LicensePoolFeatures []licensePoolFeature `json:"licensePoolFeatures"`
}

type licensePoolFeature struct {
	LicenseServerFeatureID string  `json:"licenseServerFeatureId"`
	TotalAllotment         float64 `json:"totalAllotment"`
	InUse                  float64 `json:"inUse"`
}

type activeLeasesResponse struct {
	Clients []activeLeaseClient `json:"clients"`
}

type activeLeaseClient struct {
	Leases               []activeLease                   `json:"leases"`
	AdditionalProperties activeLeaseAdditionalProperties `json:"additionalProperties"`
}

type activeLease struct {
	LeaseID                   string  `json:"leaseId"`
	FeatureName               string  `json:"featureName"`
	LeaseCount                float64 `json:"leaseCount"`
	LicenseAllotmentFeatureID string  `json:"licenseAllotmentFeatureId"`
}

type activeLeaseAdditionalProperties struct {
	LicenseServerID   string `json:"license_server_id"`
	LicenseServerName string `json:"license_server_name"`
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
