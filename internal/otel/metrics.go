package otel

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"nvidia-license-server-exporter/internal/cls"
	"nvidia-license-server-exporter/internal/snapshot"
)

const (
	defaultPushInterval   = 60 * time.Second
	defaultRefreshTimeout = 20 * time.Second

	metricUp                  = "nvidia_cls_up"
	metricScrapeDuration      = "nvidia_cls_scrape_duration_seconds"
	metricScrapeTimestamp     = "nvidia_cls_scrape_timestamp_seconds"
	metricEntitlementTotal    = "nvidia_cls_entitlement_total_quantity"
	metricServerInfo          = "nvidia_cls_license_server_info"
	metricServerFeatureTotal  = "nvidia_cls_license_server_feature_total_quantity"
	metricServerFeatureActive = "nvidia_cls_license_server_feature_active_leases"
)

type Config struct {
	Enabled           bool
	Endpoint          string
	ServiceName       string
	ServiceInstanceID string
	Insecure          bool
	PushInterval      time.Duration
	RefreshTimeout    time.Duration
}

type MetricsPusher struct {
	cfg         Config
	orgName     string
	snapshotSvc *snapshot.Service

	meterProvider *sdkmetric.MeterProvider
	cancel        context.CancelFunc
	done          chan struct{}
}

type observation struct {
	name  string
	value float64
	attrs []attribute.KeyValue
}

func NewMetricsPusher(ctx context.Context, cfg Config, orgName string, snapshotSvc *snapshot.Service) (*MetricsPusher, error) {
	cfg = normalizeConfig(cfg)
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, fmt.Errorf("otel endpoint is required")
	}
	if strings.TrimSpace(cfg.ServiceName) == "" {
		return nil, fmt.Errorf("otel service name is required")
	}

	res, err := resource.New(
		ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String(cfg.ServiceName),
			semconv.ServiceInstanceIDKey.String(cfg.ServiceInstanceID),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("create otel resource: %w", err)
	}

	expOpts := []otlpmetricgrpc.Option{
		otlpmetricgrpc.WithEndpoint(cfg.Endpoint),
	}
	if cfg.Insecure {
		expOpts = append(expOpts, otlpmetricgrpc.WithInsecure())
	}

	baseExporter, err := otlpmetricgrpc.New(ctx, expOpts...)
	if err != nil {
		return nil, fmt.Errorf("create otlp metric exporter: %w", err)
	}
	exporter := &loggingExporter{
		endpoint: cfg.Endpoint,
		exporter: baseExporter,
	}

	reader := sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(cfg.PushInterval))
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(res),
	)
	meter := meterProvider.Meter(cfg.ServiceName)

	p := &MetricsPusher{
		cfg:           cfg,
		orgName:       orgName,
		snapshotSvc:   snapshotSvc,
		meterProvider: meterProvider,
		done:          make(chan struct{}),
	}

	if err := p.registerMetrics(meter); err != nil {
		_ = meterProvider.Shutdown(ctx)
		return nil, err
	}

	return p, nil
}

func (p *MetricsPusher) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel

	go func() {
		defer close(p.done)

		p.refreshOnce()
		ticker := time.NewTicker(p.cfg.PushInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.refreshOnce()
			}
		}
	}()
}

func (p *MetricsPusher) Shutdown(ctx context.Context) error {
	if p.cancel != nil {
		p.cancel()
	}
	select {
	case <-p.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return p.meterProvider.Shutdown(ctx)
}

func (p *MetricsPusher) refreshOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), p.cfg.RefreshTimeout)
	defer cancel()

	_, _, err := p.snapshotSvc.Refresh(ctx)
	if err != nil {
		log.Printf("otel refresh failed: %v", err)
	}
}

func (p *MetricsPusher) registerMetrics(meter metric.Meter) error {
	up, err := meter.Float64ObservableGauge(metricUp)
	if err != nil {
		return fmt.Errorf("create metric nvidia_cls_up: %w", err)
	}
	scrapeDuration, err := meter.Float64ObservableGauge(metricScrapeDuration)
	if err != nil {
		return fmt.Errorf("create metric nvidia_cls_scrape_duration_seconds: %w", err)
	}
	scrapeTimestamp, err := meter.Float64ObservableGauge(metricScrapeTimestamp)
	if err != nil {
		return fmt.Errorf("create metric nvidia_cls_scrape_timestamp_seconds: %w", err)
	}
	entitlementTotal, err := meter.Float64ObservableGauge(metricEntitlementTotal)
	if err != nil {
		return fmt.Errorf("create metric nvidia_cls_entitlement_total_quantity: %w", err)
	}
	serverInfo, err := meter.Float64ObservableGauge(metricServerInfo)
	if err != nil {
		return fmt.Errorf("create metric nvidia_cls_license_server_info: %w", err)
	}
	serverFeatureTotal, err := meter.Float64ObservableGauge(metricServerFeatureTotal)
	if err != nil {
		return fmt.Errorf("create metric nvidia_cls_license_server_feature_total_quantity: %w", err)
	}
	serverFeatureActive, err := meter.Float64ObservableGauge(metricServerFeatureActive)
	if err != nil {
		return fmt.Errorf("create metric nvidia_cls_license_server_feature_active_leases: %w", err)
	}

	_, err = meter.RegisterCallback(
		func(_ context.Context, o metric.Observer) error {
			snap, meta, ok := p.snapshotSvc.Latest()
			if !ok {
				return nil
			}

			for _, item := range buildObservations(p.orgName, snap, meta) {
				switch item.name {
				case metricUp:
					o.ObserveFloat64(up, item.value, metric.WithAttributes(item.attrs...))
				case metricScrapeDuration:
					o.ObserveFloat64(scrapeDuration, item.value, metric.WithAttributes(item.attrs...))
				case metricScrapeTimestamp:
					o.ObserveFloat64(scrapeTimestamp, item.value, metric.WithAttributes(item.attrs...))
				case metricEntitlementTotal:
					o.ObserveFloat64(entitlementTotal, item.value, metric.WithAttributes(item.attrs...))
				case metricServerInfo:
					o.ObserveFloat64(serverInfo, item.value, metric.WithAttributes(item.attrs...))
				case metricServerFeatureTotal:
					o.ObserveFloat64(serverFeatureTotal, item.value, metric.WithAttributes(item.attrs...))
				case metricServerFeatureActive:
					o.ObserveFloat64(serverFeatureActive, item.value, metric.WithAttributes(item.attrs...))
				default:
					log.Printf("unknown otel metric name: %s", item.name)
				}
			}

			return nil
		},
		up,
		scrapeDuration,
		scrapeTimestamp,
		entitlementTotal,
		serverInfo,
		serverFeatureTotal,
		serverFeatureActive,
	)
	if err != nil {
		return fmt.Errorf("register otel callback: %w", err)
	}

	return nil
}

func normalizeConfig(cfg Config) Config {
	if cfg.PushInterval <= 0 {
		cfg.PushInterval = defaultPushInterval
	}
	if cfg.RefreshTimeout <= 0 {
		cfg.RefreshTimeout = defaultRefreshTimeout
	}
	return cfg
}

type loggingExporter struct {
	endpoint string
	exporter sdkmetric.Exporter
}

func (e *loggingExporter) Temporality(kind sdkmetric.InstrumentKind) metricdata.Temporality {
	return e.exporter.Temporality(kind)
}

func (e *loggingExporter) Aggregation(kind sdkmetric.InstrumentKind) sdkmetric.Aggregation {
	return e.exporter.Aggregation(kind)
}

func (e *loggingExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	if err := e.exporter.Export(ctx, rm); err != nil {
		log.Printf("otel export failed endpoint=%s err=%v", e.endpoint, err)
		return err
	}

	metricCount := 0
	for _, scopeMetrics := range rm.ScopeMetrics {
		metricCount += len(scopeMetrics.Metrics)
	}

	log.Printf("otel export succeeded endpoint=%s scopes=%d metrics=%d", e.endpoint, len(rm.ScopeMetrics), metricCount)
	return nil
}

func (e *loggingExporter) ForceFlush(ctx context.Context) error {
	return e.exporter.ForceFlush(ctx)
}

func (e *loggingExporter) Shutdown(ctx context.Context) error {
	return e.exporter.Shutdown(ctx)
}

func buildObservations(orgName string, snap *cls.Snapshot, meta snapshot.Meta) []observation {
	observations := make([]observation, 0, 3+len(snap.EntitlementFeatures)+len(snap.ServerFeatureCapacity)+len(snap.ServerFeatureActiveLeases)+len(snap.ServerUsage))
	orgAttr := attribute.String("org_name", orgName)

	observations = append(observations,
		observation{name: metricUp, value: meta.Up, attrs: []attribute.KeyValue{orgAttr}},
		observation{name: metricScrapeDuration, value: meta.DurationSeconds, attrs: []attribute.KeyValue{orgAttr}},
		observation{name: metricScrapeTimestamp, value: float64(meta.Timestamp.Unix()), attrs: []attribute.KeyValue{orgAttr}},
	)

	for _, item := range snap.EntitlementFeatures {
		observations = append(observations, observation{
			name:  metricEntitlementTotal,
			value: item.TotalQuantity,
			attrs: []attribute.KeyValue{
				orgAttr,
				attribute.String("virtual_group_id", strconv.Itoa(item.VirtualGroupID)),
				attribute.String("virtual_group_name", safeLabel(item.VirtualGroupName)),
				attribute.String("feature_name", safeLabel(item.FeatureName)),
				attribute.String("feature_version", safeLabel(item.FeatureVersion)),
				attribute.String("product_name", safeLabel(item.ProductName)),
				attribute.String("license_type", safeLabel(item.LicenseType)),
			},
		})
	}

	for _, item := range snap.ServerFeatureCapacity {
		observations = append(observations, observation{
			name:  metricServerFeatureTotal,
			value: item.TotalQuantity,
			attrs: []attribute.KeyValue{
				orgAttr,
				attribute.String("virtual_group_id", strconv.Itoa(item.VirtualGroupID)),
				attribute.String("virtual_group_name", safeLabel(item.VirtualGroupName)),
				attribute.String("server_id", safeLabel(item.ServerID)),
				attribute.String("server_name", safeLabel(item.ServerName)),
				attribute.String("feature_name", safeLabel(item.FeatureName)),
				attribute.String("product_name", safeLabel(item.ProductName)),
				attribute.String("license_type", safeLabel(item.LicenseType)),
			},
		})
	}

	for _, item := range snap.ServerFeatureActiveLeases {
		observations = append(observations, observation{
			name:  metricServerFeatureActive,
			value: item.ActiveLeases,
			attrs: []attribute.KeyValue{
				orgAttr,
				attribute.String("virtual_group_id", strconv.Itoa(item.VirtualGroupID)),
				attribute.String("virtual_group_name", safeLabel(item.VirtualGroupName)),
				attribute.String("server_id", safeLabel(item.ServerID)),
				attribute.String("server_name", safeLabel(item.ServerName)),
				attribute.String("feature_name", safeLabel(item.FeatureName)),
				attribute.String("product_name", safeLabel(item.ProductName)),
				attribute.String("license_type", safeLabel(item.LicenseType)),
			},
		})
	}

	for _, item := range snap.ServerUsage {
		observations = append(observations, observation{
			name:  metricServerInfo,
			value: 1,
			attrs: []attribute.KeyValue{
				orgAttr,
				attribute.String("virtual_group_id", strconv.Itoa(item.VirtualGroupID)),
				attribute.String("virtual_group_name", safeLabel(item.VirtualGroupName)),
				attribute.String("server_id", safeLabel(item.ServerID)),
				attribute.String("server_name", safeLabel(item.ServerName)),
				attribute.String("status", safeLabel(item.ServerStatus)),
				attribute.String("deployed_on", safeLabel(item.DeployedOn)),
				attribute.String("leasing_mode", safeLabel(item.LeasingMode)),
			},
		})
	}

	return observations
}

func safeLabel(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "unknown"
	}
	return v
}
