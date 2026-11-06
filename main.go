package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	appName          = "gcp-mig-test-app"
	metadataBaseURL  = "http://metadata.google.internal/computeMetadata/v1"
	metadataTimeout  = 2 * time.Second
	unavailableValue = "Unavailable"
)

// Config holds application configuration loaded from environment variables.
type Config struct {
	Port        string
	Version     string
	Environment string
	BuildID     string
}

// InstanceMetadata represents the GCE VM instance details returned by GET /.
type InstanceMetadata struct {
	Name        string   `json:"name"`
	ID          string   `json:"id"`
	Region      string   `json:"region"`
	Zone        string   `json:"zone"`
	MachineType string   `json:"machine_type"`
	InternalIP  string   `json:"internal_ip"`
	ExternalIP  string   `json:"external_ip"`
	Tags        []string `json:"tags"`
}

// RootResponse represents the JSON payload returned by GET /.
type RootResponse struct {
	Status         string           `json:"status"`
	Message        string           `json:"message"`
	Hostname       string           `json:"hostname"`
	Instance       InstanceMetadata `json:"instance"`
	ServiceAccount string           `json:"service_account"`
	Version        string           `json:"version"`
	Environment    string           `json:"environment"`
	BuildID        string           `json:"build_id"`
	Timestamp      string           `json:"timestamp"`
}

// StatusResponse represents the JSON payload returned by GET /health and GET /ready.
type StatusResponse struct {
	Status string `json:"status"`
}

// InfoResponse represents the JSON payload returned by GET /info.
type InfoResponse struct {
	App         string `json:"app"`
	Version     string `json:"version"`
	Environment string `json:"environment"`
	BuildID     string `json:"build_id"`
	GoVersion   string `json:"go_version"`
}

// MetadataClient queries the Google Compute Engine Metadata Server.
type MetadataClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewMetadataClient initializes a MetadataClient with a strict 2-second timeout.
func NewMetadataClient(baseURL string) *MetadataClient {
	return &MetadataClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: metadataTimeout,
		},
	}
}

// get retrieves a single metadata path from the GCE metadata server.
// If the request fails or returns a non-200 status, it logs the failure and returns "Unavailable".
func (mc *MetadataClient) get(ctx context.Context, path string) string {
	url := fmt.Sprintf("%s/%s", mc.baseURL, strings.TrimLeft(path, "/"))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		log.Printf("ERROR: failed to create metadata request for %q: %v", path, err)
		return unavailableValue
	}
	req.Header.Set("Metadata-Flavor", "Google")

	resp, err := mc.httpClient.Do(req)
	if err != nil {
		log.Printf("WARN: metadata retrieval failed for %q: %v", path, err)
		return unavailableValue
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("WARN: metadata server returned HTTP %d for %q", resp.StatusCode, path)
		return unavailableValue
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		log.Printf("WARN: failed reading metadata response body for %q: %v", path, err)
		return unavailableValue
	}

	val := strings.TrimSpace(string(body))
	if val == "" {
		return unavailableValue
	}
	return val
}

// FetchAll retrieves all required VM metadata attributes concurrently.
func (mc *MetadataClient) FetchAll(ctx context.Context) (InstanceMetadata, string) {
	paths := []string{
		"instance/name",
		"instance/id",
		"instance/zone",
		"instance/machine-type",
		"instance/network-interfaces/0/ip",
		"instance/network-interfaces/0/access-configs/0/external-ip",
		"instance/service-accounts/default/email",
		"instance/tags",
	}

	results := make(map[string]string, len(paths))
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, p := range paths {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			val := mc.get(ctx, path)
			mu.Lock()
			results[path] = val
			mu.Unlock()
		}(p)
	}

	wg.Wait()

	zone, region := parseZoneAndRegion(results["instance/zone"])
	machineType := parseLastSegment(results["instance/machine-type"])
	tags := parseTags(results["instance/tags"])

	instance := InstanceMetadata{
		Name:        results["instance/name"],
		ID:          results["instance/id"],
		Region:      region,
		Zone:        zone,
		MachineType: machineType,
		InternalIP:  results["instance/network-interfaces/0/ip"],
		ExternalIP:  results["instance/network-interfaces/0/access-configs/0/external-ip"],
		Tags:        tags,
	}

	serviceAccount := results["instance/service-accounts/default/email"]
	return instance, serviceAccount
}

// parseLastSegment extracts the final segment from a resource path such as
// "projects/123456/machineTypes/e2-medium" -> "e2-medium".
func parseLastSegment(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == unavailableValue {
		return unavailableValue
	}
	if idx := strings.LastIndex(raw, "/"); idx != -1 && idx+1 < len(raw) {
		return raw[idx+1:]
	}
	return raw
}

// parseZoneAndRegion converts "projects/123456/zones/asia-south1-a" into
// zone = "asia-south1-a" and region = "asia-south1".
func parseZoneAndRegion(rawZone string) (string, string) {
	zone := parseLastSegment(rawZone)
	if zone == unavailableValue {
		return unavailableValue, unavailableValue
	}
	if idx := strings.LastIndex(zone, "-"); idx > 0 {
		return zone, zone[:idx]
	}
	return zone, unavailableValue
}

// parseTags converts newline-separated metadata tags (or a JSON tag array) into a slice of strings.
func parseTags(rawTags string) []string {
	trimmed := strings.TrimSpace(rawTags)
	if trimmed == "" || trimmed == unavailableValue {
		return []string{}
	}

	// Support JSON array format if returned by the metadata server, otherwise split by newline.
	if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		var jsonTags []string
		if err := json.Unmarshal([]byte(trimmed), &jsonTags); err == nil {
			if jsonTags == nil {
				return []string{}
			}
			return jsonTags
		}
	}

	lines := strings.Split(trimmed, "\n")
	tags := make([]string, 0, len(lines))
	for _, line := range lines {
		tag := strings.TrimSpace(line)
		if tag != "" {
			tags = append(tags, tag)
		}
	}
	return tags
}

// loadConfig loads application configuration from environment variables with defaults.
func loadConfig() Config {
	return Config{
		Port:        getEnv("PORT", "8080"),
		Version:     getEnv("VERSION", "v1.0.0"),
		Environment: getEnv("ENVIRONMENT", "dev"),
		BuildID:     getEnv("BUILD_ID", "local"),
	}
}

func getEnv(key, fallback string) string {
	if val, ok := os.LookupEnv(key); ok && strings.TrimSpace(val) != "" {
		return strings.TrimSpace(val)
	}
	return fallback
}

// Server wraps application dependencies and HTTP handlers.
type Server struct {
	cfg      Config
	metadata *MetadataClient
}

// NewServer constructs a new Server instance.
func NewServer(cfg Config, metadata *MetadataClient) *Server {
	return &Server{
		cfg:      cfg,
		metadata: metadata,
	}
}

// routes registers all HTTP endpoints and wraps them with request logging middleware.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/ready", s.handleReady)
	mux.HandleFunc("/info", s.handleInfo)

	return loggingMiddleware(mux)
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"status":  "error",
			"message": "endpoint not found",
		})
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"status":  "error",
			"message": "method not allowed",
		})
		return
	}

	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		log.Printf("WARN: failed to determine local hostname: %v", err)
		hostname = unavailableValue
	}

	instance, serviceAccount := s.metadata.FetchAll(r.Context())

	resp := RootResponse{
		Status:         "success",
		Message:        "Request successfully served by GCP MIG",
		Hostname:       hostname,
		Instance:       instance,
		ServiceAccount: serviceAccount,
		Version:        s.cfg.Version,
		Environment:    s.cfg.Environment,
		BuildID:        s.cfg.BuildID,
		Timestamp:      time.Now().UTC().Format(time.RFC3339),
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"status":  "error",
			"message": "method not allowed",
		})
		return
	}

	writeJSON(w, http.StatusOK, StatusResponse{Status: "healthy"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"status":  "error",
			"message": "method not allowed",
		})
		return
	}

	writeJSON(w, http.StatusOK, StatusResponse{Status: "ready"})
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"status":  "error",
			"message": "method not allowed",
		})
		return
	}

	writeJSON(w, http.StatusOK, InfoResponse{
		App:         appName,
		Version:     s.cfg.Version,
		Environment: s.cfg.Environment,
		BuildID:     s.cfg.BuildID,
		GoVersion:   runtime.Version(),
	})
}

// writeJSON serializes payload aspretty-printed JSON and writes it to the HTTP response.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(payload); err != nil {
		log.Printf("ERROR: failed to encode JSON response: %v", err)
	}
}

// statusRecorder wraps http.ResponseWriter to capture the HTTP status code for logging.
type statusRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.statusCode = code
	sr.ResponseWriter.WriteHeader(code)
}

// loggingMiddleware logs every incoming HTTP request with method, path, remote address, status, and duration.
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{
			ResponseWriter: w,
			statusCode:     http.StatusOK,
		}

		next.ServeHTTP(rec, r)

		log.Printf("INFO: request method=%s path=%s remote_addr=%s status=%d duration=%s user_agent=%q",
			r.Method, r.URL.Path, r.RemoteAddr, rec.statusCode, time.Since(start).Round(time.Microsecond), r.UserAgent())
	})
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC | log.Lmsgprefix)

	cfg := loadConfig()
	metadataClient := NewMetadataClient(metadataBaseURL)
	srv := NewServer(cfg, metadataClient)

	httpServer := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	shutdownCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("INFO: starting %s on port %s (version=%s, environment=%s, build_id=%s, go_version=%s)",
			appName, cfg.Port, cfg.Version, cfg.Environment, cfg.BuildID, runtime.Version())

		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("FATAL: HTTP server failed: %v", err)
		}
	}()

	<-shutdownCtx.Done()
	log.Printf("INFO: shutdown signal received, stopping %s gracefully...", appName)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("ERROR: graceful shutdown failed: %v", err)
	} else {
		log.Printf("INFO: %s stopped cleanly", appName)
	}
}
