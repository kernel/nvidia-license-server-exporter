package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"nvidia-license-server-exporter/internal/cls"
	"nvidia-license-server-exporter/internal/exporter"
	"nvidia-license-server-exporter/internal/otel"
	"nvidia-license-server-exporter/internal/snapshot"
)

func main() {
	var (
		listenAddress = flag.String("listen-address", defaultListenAddress(), "Address to listen on for HTTP requests.")
		metricsPath   = flag.String("metrics-path", getenv("METRICS_PATH", "/metrics"), "Path where metrics are exposed.")
		baseURL       = flag.String("nvidia-api-base-url", getenv("NVIDIA_API_BASE_URL", "https://api.licensing.nvidia.com"), "NVIDIA CLS API base URL.")
		orgName       = flag.String("nvidia-org-name", firstNonEmpty(getenv("NVIDIA_ORG_NAME", ""), getenv("NLS_ORG_NAME", "")), "NVIDIA org name / ID (e.g. lic-...).")
		apiKey        = flag.String("nvidia-api-key", firstNonEmpty(getenv("NVIDIA_API_KEY", ""), getenv("NLS_API_KEY", "")), "NVIDIA Licensing State API key.")
		serviceID     = flag.String("nvidia-service-instance-id", getenv("NVIDIA_SERVICE_INSTANCE_ID", ""), "Optional service instance ID sent as x-nv-service-instance-id.")
		scrapeTimeout = flag.Duration("scrape-timeout", durationFromEnv("SCRAPE_TIMEOUT", 20*time.Second), "Timeout for each CLS scrape.")
		cacheTTL      = flag.Duration("cache-ttl", durationFromEnv("CACHE_TTL", 60*time.Second), "In-memory cache TTL for CLS snapshots.")
		parallelism   = flag.Int("parallelism", intFromEnv("PARALLELISM", 8), "Max concurrent CLS API calls during scrape.")
		otelEnabled   = flag.Bool("otel-enabled", boolFromEnv("OTEL_ENABLED", false), "Enable OTEL metrics export.")
		otelEndpoint  = flag.String("otel-endpoint", getenv("OTEL_ENDPOINT", "127.0.0.1:4317"), "OTLP gRPC endpoint.")
		otelSvcName   = flag.String("otel-service-name", getenv("OTEL_SERVICE_NAME", "nvidia-license-server-exporter"), "OTEL service.name.")
		otelSvcID     = flag.String("otel-service-instance-id", getenv("OTEL_SERVICE_INSTANCE_ID", hostnameOrUnknown()), "OTEL service.instance.id.")
		otelInsecure  = flag.Bool("otel-insecure", boolFromEnv("OTEL_INSECURE", true), "Disable TLS for OTLP.")
		otelInterval  = flag.Duration("otel-push-interval", durationFromEnv("OTEL_PUSH_INTERVAL", 60*time.Second), "OTEL periodic push interval.")
	)
	flag.Parse()

	if strings.TrimSpace(*orgName) == "" {
		log.Fatal("missing required org name: set NVIDIA_ORG_NAME or pass -nvidia-org-name")
	}
	if strings.TrimSpace(*apiKey) == "" {
		log.Fatal("missing required API key: set NVIDIA_API_KEY or pass -nvidia-api-key")
	}

	client, err := cls.NewClient(cls.Config{
		BaseURL:           *baseURL,
		APIKey:            *apiKey,
		OrgName:           *orgName,
		ServiceInstanceID: *serviceID,
		ParallelFetches:   *parallelism,
	})
	if err != nil {
		log.Fatalf("failed to create CLS client: %v", err)
	}

	snapshotSvc := snapshot.NewService(client, *cacheTTL)

	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		exporter.NewCollector(snapshotSvc, *orgName, *scrapeTimeout),
	)

	mux := http.NewServeMux()
	mux.Handle(*metricsPath, promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "nvidia-license-server-exporter\nscrape metrics at %s\n", *metricsPath)
	})
	handler := loggingMiddleware(recoverMiddleware(mux))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var otelPusher *otel.MetricsPusher
	if *otelEnabled {
		pusher, initErr := otel.NewMetricsPusher(ctx, otel.Config{
			Enabled:           *otelEnabled,
			Endpoint:          *otelEndpoint,
			ServiceName:       *otelSvcName,
			ServiceInstanceID: *otelSvcID,
			Insecure:          *otelInsecure,
			PushInterval:      *otelInterval,
			RefreshTimeout:    *scrapeTimeout,
		}, *orgName, snapshotSvc)
		if initErr != nil {
			log.Fatalf("failed to initialize otel metrics: %v", initErr)
		}
		otelPusher = pusher
		otelPusher.Start()
		log.Printf("otel enabled endpoint=%s insecure=%t interval=%s", *otelEndpoint, *otelInsecure, otelInterval.String())
	}

	server := &http.Server{
		Addr:    *listenAddress,
		Handler: handler,
		ErrorLog: log.New(os.Stderr, "http-server ", log.LstdFlags|log.LUTC),
	}

	log.Printf("starting nvidia-license-server-exporter on %s", *listenAddress)
	log.Printf("scraping org=%s base_url=%s", *orgName, *baseURL)
	log.Printf("cache_ttl=%s", cacheTTL.String())

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.ListenAndServe()
	}()

	select {
	case err := <-serverErr:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server failed: %v", err)
		}
	case <-ctx.Done():
		log.Printf("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if otelPusher != nil {
		if err := otelPusher.Shutdown(shutdownCtx); err != nil {
			log.Printf("otel shutdown error: %v", err)
		}
	}
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("http shutdown error: %v", err)
	}
}

type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
	bytes      int
}

func (w *loggingResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *loggingResponseWriter) Write(p []byte) (int, error) {
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += n
	return n, err
}

func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic recovered method=%s path=%s err=%v", r.Method, r.URL.Path, rec)
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lw := &loggingResponseWriter{
			ResponseWriter: w,
			statusCode:     http.StatusOK,
		}

		next.ServeHTTP(lw, r)

		log.Printf(
			"http request method=%s path=%s status=%d bytes=%d duration=%s remote=%s user_agent=%q",
			r.Method,
			r.URL.Path,
			lw.statusCode,
			lw.bytes,
			time.Since(start).String(),
			r.RemoteAddr,
			r.UserAgent(),
		)
	})
}

func getenv(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func intFromEnv(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	var value int
	_, err := fmt.Sscanf(raw, "%d", &value)
	if err != nil {
		return fallback
	}
	return value
}

func boolFromEnv(key string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return value
}

func durationFromEnv(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return value
}

func hostnameOrUnknown() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		return "unknown"
	}
	return host
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func defaultListenAddress() string {
	if v := strings.TrimSpace(os.Getenv("LISTEN_ADDRESS")); v != "" {
		return v
	}

	port := strings.TrimSpace(os.Getenv("PORT"))
	if port != "" {
		if strings.HasPrefix(port, ":") {
			return port
		}
		return ":" + port
	}

	return ":9844"
}
