package exporter

import (
	"context"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"nvidia-license-server-exporter/internal/snapshot"
)

type Collector struct {
	snapshotSvc   *snapshot.Service
	scrapeTimeout time.Duration

	upDesc                  *prometheus.Desc
	scrapeDurationDesc      *prometheus.Desc
	scrapeTimestampDesc     *prometheus.Desc
	entitlementTotalDesc    *prometheus.Desc
	serverInfoDesc          *prometheus.Desc
	serverFeatureCapacity   *prometheus.Desc
	serverFeatureActiveDesc *prometheus.Desc

	descs []*prometheus.Desc
}

func NewCollector(snapshotSvc *snapshot.Service, orgName string, scrapeTimeout time.Duration) *Collector {
	constLabel := prometheus.Labels{"org_name": orgName}

	c := &Collector{
		snapshotSvc:   snapshotSvc,
		scrapeTimeout: scrapeTimeout,

		upDesc: prometheus.NewDesc(
			"nvidia_cls_up",
			"Whether the NVIDIA CLS scrape is successful (1 = up, 0 = down).",
			nil,
			constLabel,
		),
		scrapeDurationDesc: prometheus.NewDesc(
			"nvidia_cls_scrape_duration_seconds",
			"Time spent querying NVIDIA CLS APIs.",
			nil,
			constLabel,
		),
		scrapeTimestampDesc: prometheus.NewDesc(
			"nvidia_cls_scrape_timestamp_seconds",
			"Unix timestamp for when the scrape snapshot was collected.",
			nil,
			constLabel,
		),
		entitlementTotalDesc: prometheus.NewDesc(
			"nvidia_cls_entitlement_total_quantity",
			"Total entitlement quantity by virtual group and feature (contract capacity).",
			[]string{"virtual_group_id", "virtual_group_name", "feature_name", "feature_version", "product_name", "license_type"},
			constLabel,
		),
		serverInfoDesc: prometheus.NewDesc(
			"nvidia_cls_license_server_info",
			"Static information about a license server.",
			[]string{"virtual_group_id", "virtual_group_name", "server_id", "server_name", "status", "deployed_on", "leasing_mode"},
			constLabel,
		),
		serverFeatureCapacity: prometheus.NewDesc(
			"nvidia_cls_license_server_feature_total_quantity",
			"Total server feature capacity from license-server features.",
			[]string{"virtual_group_id", "virtual_group_name", "server_id", "server_name", "feature_name", "product_name", "license_type"},
			constLabel,
		),
		serverFeatureActiveDesc: prometheus.NewDesc(
			"nvidia_cls_license_server_feature_active_leases",
			"Active lease count by server feature from CLS active-lease data.",
			[]string{"virtual_group_id", "virtual_group_name", "server_id", "server_name", "feature_name", "product_name", "license_type"},
			constLabel,
		),
	}

	c.descs = []*prometheus.Desc{
		c.upDesc,
		c.scrapeDurationDesc,
		c.scrapeTimestampDesc,
		c.entitlementTotalDesc,
		c.serverInfoDesc,
		c.serverFeatureCapacity,
		c.serverFeatureActiveDesc,
	}

	return c
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range c.descs {
		ch <- desc
	}
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), c.scrapeTimeout)
	defer cancel()

	snapshot, meta, err := c.snapshotSvc.Get(ctx)
	if err != nil {
		log.Printf("cls scrape failed: %v", err)
		lastMeta := c.snapshotSvc.Meta()
		ch <- prometheus.MustNewConstMetric(c.upDesc, prometheus.GaugeValue, 0)
		ch <- prometheus.MustNewConstMetric(c.scrapeDurationDesc, prometheus.GaugeValue, lastMeta.DurationSeconds)
		if !lastMeta.Timestamp.IsZero() {
			ch <- prometheus.MustNewConstMetric(c.scrapeTimestampDesc, prometheus.GaugeValue, float64(lastMeta.Timestamp.Unix()))
		}
		return
	}

	ch <- prometheus.MustNewConstMetric(c.upDesc, prometheus.GaugeValue, meta.Up)
	ch <- prometheus.MustNewConstMetric(c.scrapeDurationDesc, prometheus.GaugeValue, meta.DurationSeconds)
	ch <- prometheus.MustNewConstMetric(c.scrapeTimestampDesc, prometheus.GaugeValue, float64(meta.Timestamp.Unix()))

	for _, item := range snapshot.EntitlementFeatures {
		labels := []string{
			strconv.Itoa(item.VirtualGroupID),
			safeLabel(item.VirtualGroupName),
			safeLabel(item.FeatureName),
			safeLabel(item.FeatureVersion),
			safeLabel(item.ProductName),
			safeLabel(item.LicenseType),
		}
		ch <- prometheus.MustNewConstMetric(c.entitlementTotalDesc, prometheus.GaugeValue, item.TotalQuantity, labels...)
	}

	for _, item := range snapshot.ServerFeatureCapacity {
		labels := []string{
			strconv.Itoa(item.VirtualGroupID),
			safeLabel(item.VirtualGroupName),
			safeLabel(item.ServerID),
			safeLabel(item.ServerName),
			safeLabel(item.FeatureName),
			safeLabel(item.ProductName),
			safeLabel(item.LicenseType),
		}
		ch <- prometheus.MustNewConstMetric(c.serverFeatureCapacity, prometheus.GaugeValue, item.TotalQuantity, labels...)
	}

	for _, item := range snapshot.ServerFeatureActiveLeases {
		labels := []string{
			strconv.Itoa(item.VirtualGroupID),
			safeLabel(item.VirtualGroupName),
			safeLabel(item.ServerID),
			safeLabel(item.ServerName),
			safeLabel(item.FeatureName),
			safeLabel(item.ProductName),
			safeLabel(item.LicenseType),
		}
		ch <- prometheus.MustNewConstMetric(c.serverFeatureActiveDesc, prometheus.GaugeValue, item.ActiveLeases, labels...)
	}

	for _, item := range snapshot.ServerUsage {
		infoLabels := []string{
			strconv.Itoa(item.VirtualGroupID),
			safeLabel(item.VirtualGroupName),
			safeLabel(item.ServerID),
			safeLabel(item.ServerName),
			safeLabel(item.ServerStatus),
			safeLabel(item.DeployedOn),
			safeLabel(item.LeasingMode),
		}
		ch <- prometheus.MustNewConstMetric(c.serverInfoDesc, prometheus.GaugeValue, 1, infoLabels...)
	}
}

func safeLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	return value
}
