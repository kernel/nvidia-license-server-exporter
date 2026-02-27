package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"nvidia-license-server-exporter/internal/cls"
	"nvidia-license-server-exporter/internal/exporter"
)

func main() {
	var (
		listenAddress = flag.String("listen-address", getenv("LISTEN_ADDRESS", ":9844"), "Address to listen on for HTTP requests.")
		metricsPath   = flag.String("metrics-path", getenv("METRICS_PATH", "/metrics"), "Path where metrics are exposed.")
		baseURL       = flag.String("nvidia-api-base-url", getenv("NVIDIA_API_BASE_URL", "https://api.licensing.nvidia.com"), "NVIDIA CLS API base URL.")
		orgName       = flag.String("nvidia-org-name", firstNonEmpty(getenv("NVIDIA_ORG_NAME", ""), getenv("NLS_ORG_NAME", "")), "NVIDIA org name / ID (e.g. lic-...).")
		apiKey        = flag.String("nvidia-api-key", firstNonEmpty(getenv("NVIDIA_API_KEY", ""), getenv("NLS_API_KEY", "")), "NVIDIA Licensing State API key.")
		serviceID     = flag.String("nvidia-service-instance-id", getenv("NVIDIA_SERVICE_INSTANCE_ID", ""), "Optional service instance ID sent as x-nv-service-instance-id.")
		scrapeTimeout = flag.Duration("scrape-timeout", durationFromEnv("SCRAPE_TIMEOUT", 20*time.Second), "Timeout for each CLS scrape.")
		cacheTTL      = flag.Duration("cache-ttl", durationFromEnv("CACHE_TTL", 60*time.Second), "In-memory cache TTL for CLS snapshots.")
		parallelism   = flag.Int("parallelism", intFromEnv("PARALLELISM", 8), "Max concurrent CLS API calls during scrape.")
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

	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		exporter.NewCollector(client, *scrapeTimeout, *cacheTTL),
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

	log.Printf("starting nvidia-license-server-exporter on %s", *listenAddress)
	log.Printf("scraping org=%s base_url=%s", *orgName, *baseURL)
	log.Printf("cache_ttl=%s", cacheTTL.String())
	if err := http.ListenAndServe(*listenAddress, mux); err != nil {
		log.Fatal(err)
	}
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
