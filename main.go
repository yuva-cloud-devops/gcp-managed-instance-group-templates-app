package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
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
	tmpl     *template.Template
}

// NewServer constructs a new Server instance.
func NewServer(cfg Config, metadata *MetadataClient) *Server {
	tmpl := template.Must(template.New("dashboard").Parse(htmlDashboardTemplate))
	return &Server{
		cfg:      cfg,
		metadata: metadata,
		tmpl:     tmpl,
	}
}

// routes registers all HTTP endpoints and wraps them with request logging middleware.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/api", s.handleAPIRoot)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/ready", s.handleReady)
	mux.HandleFunc("/info", s.handleInfo)

	return loggingMiddleware(mux)
}

func (s *Server) buildRootResponse(ctx context.Context) RootResponse {
	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		log.Printf("WARN: failed to determine local hostname: %v", err)
		hostname = unavailableValue
	}

	instance, serviceAccount := s.metadata.FetchAll(ctx)

	return RootResponse{
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

	resp := s.buildRootResponse(r.Context())

	// If client explicitly requests JSON (e.g. curl, fetch with ?format=json or Accept: application/json)
	accept := r.Header.Get("Accept")
	formatQuery := r.URL.Query().Get("format")
	if formatQuery == "json" || (strings.Contains(accept, "application/json") && !strings.Contains(accept, "text/html")) {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// For standard browser requests, render the interactive HTML dashboard
	rawJSON, _ := json.MarshalIndent(resp, "", "  ")
	templateData := struct {
		RootResponse
		RawJSON   string
		GoVersion string
	}{
		RootResponse: resp,
		RawJSON:      string(rawJSON),
		GoVersion:    runtime.Version(),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := s.tmpl.Execute(w, templateData); err != nil {
		log.Printf("ERROR: failed to render HTML template: %v", err)
	}
}

func (s *Server) handleAPIRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"status":  "error",
			"message": "method not allowed",
		})
		return
	}
	resp := s.buildRootResponse(r.Context())
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

// writeJSON serializes payload as pretty-printed JSON and writes it to the HTTP response.
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

// Self-contained, responsive, clean dark-theme HTML Dashboard (zero external assets/CDNs needed).
const htmlDashboardTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>GCP MIG Dashboard | {{.Instance.Name}}</title>
  <style>
    :root {
      --bg: #0d1117;
      --card-bg: #161b22;
      --card-border: #30363d;
      --text-main: #f0f6fc;
      --text-muted: #8b949e;
      --accent-blue: #58a6ff;
      --accent-green: #3fb950;
      --accent-purple: #bc8cff;
      --accent-orange: #d29922;
      --accent-cyan: #39c5cf;
      --tag-bg: #21262d;
    }
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body {
      background-color: var(--bg);
      color: var(--text-main);
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
      line-height: 1.5;
      padding: 24px;
    }
    .container { max-width: 1100px; margin: 0 auto; }
    
    /* Header */
    header {
      display: flex;
      justify-content: space-between;
      align-items: center;
      flex-wrap: wrap;
      gap: 16px;
      padding-bottom: 24px;
      border-bottom: 1px solid var(--card-border);
      margin-bottom: 24px;
    }
    .brand { display: flex; align-items: center; gap: 12px; }
    .brand-icon {
      width: 40px; height: 40px;
      background: linear-gradient(135deg, #4285F4, #34A853);
      border-radius: 8px;
      display: flex; align-items: center; justify-content: center;
      font-weight: bold; font-size: 20px; color: #fff;
    }
    .brand h1 { font-size: 22px; font-weight: 700; }
    .brand p { font-size: 13px; color: var(--text-muted); }

    .header-badges { display: flex; gap: 8px; align-items: center; }
    .badge {
      display: inline-flex; align-items: center; gap: 6px;
      padding: 4px 10px; border-radius: 20px; font-size: 12px; font-weight: 600;
    }
    .badge-green { background: rgba(63, 185, 80, 0.15); color: var(--accent-green); border: 1px solid rgba(63, 185, 80, 0.3); }
    .badge-blue { background: rgba(88, 166, 255, 0.15); color: var(--accent-blue); border: 1px solid rgba(88, 166, 255, 0.3); }
    .badge-orange { background: rgba(210, 153, 34, 0.15); color: var(--accent-orange); border: 1px solid rgba(210, 153, 34, 0.3); }
    .dot { width: 8px; height: 8px; border-radius: 50%; background-color: currentColor; }
    .pulse { animation: pulse 2s infinite; }
    @keyframes pulse { 0% { opacity: 1; } 50% { opacity: 0.3; } 100% { opacity: 1; } }

    /* Controls Bar */
    .controls {
      display: flex; justify-content: space-between; align-items: center;
      background: var(--card-bg); border: 1px solid var(--card-border);
      border-radius: 8px; padding: 12px 16px; margin-bottom: 24px; flex-wrap: wrap; gap: 12px;
    }
    .btn {
      background: #238636; color: white; border: none; padding: 6px 14px;
      border-radius: 6px; font-size: 13px; font-weight: 600; cursor: pointer;
      display: inline-flex; align-items: center; gap: 6px; text-decoration: none;
      transition: background-color 0.2s;
    }
    .btn:hover { background: #2ea043; }
    .btn-secondary { background: #21262d; border: 1px solid var(--card-border); color: var(--text-main); }
    .btn-secondary:hover { background: #30363d; }
    .btn-active { background: #1f6feb !important; color: #fff !important; border-color: #388bfd !important; }
    
    .poll-toggle { display: flex; align-items: center; gap: 8px; font-size: 13px; color: var(--text-muted); cursor: pointer; }

    /* Endpoint Explorer Bar */
    .endpoint-bar {
      background: var(--card-bg);
      border: 1px solid var(--card-border);
      border-radius: 8px;
      padding: 16px;
      margin-bottom: 24px;
    }
    .endpoint-bar-header {
      display: flex; justify-content: space-between; align-items: center;
      margin-bottom: 12px; font-size: 13px; font-weight: 600; text-transform: uppercase;
      letter-spacing: 0.5px; color: var(--text-muted);
    }
    .endpoint-buttons {
      display: flex; flex-wrap: wrap; gap: 10px; align-items: center;
    }
    .btn-endpoint {
      background: #21262d; border: 1px solid var(--card-border);
      color: var(--text-main); padding: 8px 14px; border-radius: 6px;
      font-size: 13px; font-weight: 600; cursor: pointer; display: inline-flex;
      align-items: center; gap: 8px; font-family: ui-monospace, monospace;
    }
    .btn-endpoint:hover { background: #30363d; border-color: var(--accent-blue); }
    .method-get { color: var(--accent-green); font-weight: 700; font-size: 11px; }

    /* Grid Layout */
    .grid {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(320px, 1fr));
      gap: 16px;
      margin-bottom: 24px;
    }
    .card {
      background: var(--card-bg);
      border: 1px solid var(--card-border);
      border-radius: 8px;
      padding: 18px;
    }
    .card-title {
      font-size: 13px; text-transform: uppercase; letter-spacing: 0.5px;
      color: var(--text-muted); margin-bottom: 14px; font-weight: 600;
      display: flex; align-items: center; justify-content: space-between;
    }
    .card-title svg { width: 16px; height: 16px; fill: currentColor; }
    
    .data-row {
      display: flex; justify-content: space-between; align-items: flex-start;
      padding: 8px 0; border-bottom: 1px solid rgba(48, 54, 61, 0.4);
      font-size: 14px;
    }
    .data-row:last-child { border-bottom: none; }
    .data-label { color: var(--text-muted); flex-shrink: 0; }
    .data-value {
      font-family: ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace;
      color: var(--text-main); word-break: break-all; text-align: right; font-weight: 500;
    }
    .highlight-name { color: var(--accent-blue); font-weight: 700; font-size: 15px; }
    .highlight-zone { color: var(--accent-cyan); font-weight: 600; }
    
    .tags-container { display: flex; flex-wrap: wrap; gap: 6px; justify-content: flex-end; }
    .tag-chip {
      background: var(--tag-bg); border: 1px solid var(--card-border);
      padding: 2px 8px; border-radius: 12px; font-size: 12px; color: var(--accent-purple);
      font-family: monospace;
    }

    /* JSON & Raw Section */
    .json-section {
      background: var(--card-bg); border: 1px solid var(--card-border);
      border-radius: 8px; padding: 18px; margin-bottom: 24px;
    }
    .json-header {
      display: flex; justify-content: space-between; align-items: center; margin-bottom: 12px;
      flex-wrap: wrap; gap: 8px;
    }
    .json-title-group { display: flex; align-items: center; gap: 10px; }
    .status-pill {
      font-size: 11px; font-weight: 700; padding: 2px 8px; border-radius: 10px;
      background: rgba(63, 185, 80, 0.2); color: var(--accent-green); border: 1px solid rgba(63, 185, 80, 0.4);
    }
    pre {
      background: #0d1117; padding: 14px; border-radius: 6px;
      border: 1px solid var(--card-border); overflow-x: auto;
      font-family: ui-monospace, monospace; font-size: 13px; color: #7ee787;
    }

    /* Quick Links Footer */
    footer {
      display: flex; justify-content: space-between; align-items: center;
      flex-wrap: wrap; gap: 12px; color: var(--text-muted); font-size: 13px;
      padding-top: 16px; border-top: 1px solid var(--card-border);
    }
    .footer-links { display: flex; gap: 16px; }
    .footer-links a { color: var(--accent-blue); text-decoration: none; }
    .footer-links a:hover { text-decoration: underline; }
  </style>
</head>
<body>
  <div class="container">
    <header>
      <div class="brand">
        <div class="brand-icon">G</div>
        <div>
          <h1>GCP Managed Instance Group Validator</h1>
          <p>Real-time VM Identity & Golden Image Verification</p>
        </div>
      </div>
      <div class="header-badges">
        <span class="badge badge-green"><span class="dot pulse"></span> Healthy</span>
        <span class="badge badge-blue">Env: {{.Environment}}</span>
        <span class="badge badge-orange">Ver: {{.Version}}</span>
      </div>
    </header>

    <!-- Controls Bar -->
    <div class="controls">
      <div>
        <span style="font-size: 13px; color: var(--text-muted);">Last Served At:</span>
        <strong id="timestamp-display" style="font-size: 13px; margin-left: 6px;">{{.Timestamp}}</strong>
      </div>
      <div style="display: flex; gap: 12px; align-items: center;">
        <label class="poll-toggle">
          <input type="checkbox" id="auto-refresh-toggle">
          <span>Auto-Poll (2s)</span>
        </label>
        <button class="btn btn-secondary" onclick="fetchLiveMetadata()">↻ Refresh Now</button>
      </div>
    </div>

    <!-- Interactive API Endpoint Explorer Buttons -->
    <div class="endpoint-bar">
      <div class="endpoint-bar-header">
        <span>Interactive API Endpoint Tester</span>
        <span id="endpoint-meta-info" style="font-size: 12px; color: var(--accent-cyan); text-transform: none; font-weight: normal;">Click any endpoint to test response</span>
      </div>
      <div class="endpoint-buttons">
        <button class="btn-endpoint btn-active" id="btn-ep-root" onclick="callEndpoint('/', this)">
          <span class="method-get">GET</span> /
        </button>
        <button class="btn-endpoint" id="btn-ep-api" onclick="callEndpoint('/api', this)">
          <span class="method-get">GET</span> /api
        </button>
        <button class="btn-endpoint" id="btn-ep-health" onclick="callEndpoint('/health', this)">
          <span class="method-get">GET</span> /health
        </button>
        <button class="btn-endpoint" id="btn-ep-ready" onclick="callEndpoint('/ready', this)">
          <span class="method-get">GET</span> /ready
        </button>
        <button class="btn-endpoint" id="btn-ep-info" onclick="callEndpoint('/info', this)">
          <span class="method-get">GET</span> /info
        </button>
      </div>
    </div>

    <!-- Cards Grid -->
    <div class="grid">
      <!-- Card 1: Instance Identity -->
      <div class="card">
        <div class="card-title">Instance Identity</div>
        <div class="data-row">
          <span class="data-label">Instance Name</span>
          <span class="data-value highlight-name" id="val-instance-name">{{.Instance.Name}}</span>
        </div>
        <div class="data-row">
          <span class="data-label">Instance ID</span>
          <span class="data-value" id="val-instance-id">{{.Instance.ID}}</span>
        </div>
        <div class="data-row">
          <span class="data-label">Hostname</span>
          <span class="data-value" id="val-hostname">{{.Hostname}}</span>
        </div>
        <div class="data-row">
          <span class="data-label">Machine Type</span>
          <span class="data-value" id="val-machine-type">{{.Instance.MachineType}}</span>
        </div>
      </div>

      <!-- Card 2: GCP Topology & Network -->
      <div class="card">
        <div class="card-title">GCP Topology & Network</div>
        <div class="data-row">
          <span class="data-label">Region</span>
          <span class="data-value" id="val-region">{{.Instance.Region}}</span>
        </div>
        <div class="data-row">
          <span class="data-label">Zone</span>
          <span class="data-value highlight-zone" id="val-zone">{{.Instance.Zone}}</span>
        </div>
        <div class="data-row">
          <span class="data-label">Internal IP</span>
          <span class="data-value" id="val-internal-ip">{{.Instance.InternalIP}}</span>
        </div>
        <div class="data-row">
          <span class="data-label">External IP</span>
          <span class="data-value" id="val-external-ip">{{.Instance.ExternalIP}}</span>
        </div>
      </div>

      <!-- Card 3: Build & Provenance -->
      <div class="card">
        <div class="card-title">Build & Provenance</div>
        <div class="data-row">
          <span class="data-label">Build ID</span>
          <span class="data-value" id="val-build-id">{{.BuildID}}</span>
        </div>
        <div class="data-row">
          <span class="data-label">Version</span>
          <span class="data-value" id="val-version">{{.Version}}</span>
        </div>
        <div class="data-row">
          <span class="data-label">Environment</span>
          <span class="data-value" id="val-environment">{{.Environment}}</span>
        </div>
        <div class="data-row">
          <span class="data-label">Go Version</span>
          <span class="data-value">{{.GoVersion}}</span>
        </div>
      </div>

      <!-- Card 4: Security & Tags -->
      <div class="card">
        <div class="card-title">Security & Tags</div>
        <div class="data-row">
          <span class="data-label">Service Account</span>
          <span class="data-value" id="val-sa" style="font-size: 12px;">{{.ServiceAccount}}</span>
        </div>
        <div class="data-row">
          <span class="data-label">Network Tags</span>
          <div class="tags-container" id="val-tags">
            {{range .Instance.Tags}}
              <span class="tag-chip">{{.}}</span>
            {{else}}
              <span class="data-value">None</span>
            {{end}}
          </div>
        </div>
      </div>
    </div>

    <!-- Live JSON Payload Section -->
    <div class="json-section">
      <div class="json-header">
        <div class="json-title-group">
          <span style="font-size: 13px; font-weight: 600; text-transform: uppercase; color: var(--text-muted);">Response Payload</span>
          <span class="status-pill" id="response-status-pill">HTTP 200 OK</span>
          <span id="response-endpoint-name" style="font-size: 12px; color: var(--text-muted); font-family: monospace;">GET /</span>
        </div>
        <div style="display: flex; gap: 8px;">
          <a class="btn btn-secondary" style="padding: 3px 10px; font-size: 12px;" id="open-raw-link" href="/" target="_blank">Open in Tab ↗</a>
          <button class="btn btn-secondary" style="padding: 3px 10px; font-size: 12px;" onclick="copyRawJSON()">Copy JSON</button>
        </div>
      </div>
      <pre><code id="json-code-block">{{.RawJSON}}</code></pre>
    </div>

    <footer>
      <div>Application: <strong>gcp-mig-test-app</strong></div>
      <div class="footer-links">
        <a href="/api" target="_blank">/api</a>
        <a href="/health" target="_blank">/health</a>
        <a href="/ready" target="_blank">/ready</a>
        <a href="/info" target="_blank">/info</a>
      </div>
    </footer>
  </div>

  <script>
    let pollInterval = null;
    let currentEndpoint = '/';

    async function callEndpoint(path, btnElem) {
      if (btnElem) {
        document.querySelectorAll('.btn-endpoint').forEach(b => b.classList.remove('btn-active'));
        btnElem.classList.add('btn-active');
      }
      currentEndpoint = path;
      document.getElementById('open-raw-link').href = path;
      document.getElementById('response-endpoint-name').innerText = 'GET ' + path;

      const startTime = performance.now();
      try {
        const fetchUrl = (path === '/' || path === '') ? '/?format=json' : path;
        const res = await fetch(fetchUrl, {
          headers: { 'Accept': 'application/json' }
        });
        const duration = Math.round(performance.now() - startTime);
        const data = await res.json();

        // Update Status Pill
        const pill = document.getElementById('response-status-pill');
        pill.innerText = 'HTTP ' + res.status + ' OK (' + duration + 'ms)';
        pill.style.background = res.ok ? 'rgba(63, 185, 80, 0.2)' : 'rgba(248, 81, 73, 0.2)';
        pill.style.color = res.ok ? 'var(--accent-green)' : '#f85149';

        document.getElementById('endpoint-meta-info').innerText = 'Called GET ' + path + ' in ' + duration + 'ms';

        // Update JSON Code block
        document.getElementById('json-code-block').innerText = JSON.stringify(data, null, 2);

        // If it's root or api, also update the cards
        if (path === '/' || path === '/api') {
          updateCards(data);
        }
      } catch (err) {
        console.error('Endpoint call failed:', err);
        const pill = document.getElementById('response-status-pill');
        pill.innerText = 'ERROR';
        pill.style.background = 'rgba(248, 81, 73, 0.2)';
        pill.style.color = '#f85149';
      }
    }

    function updateCards(data) {
      if (!data) return;
      document.getElementById('timestamp-display').innerText = data.timestamp || '';
      document.getElementById('val-instance-name').innerText = data.instance?.name || 'Unavailable';
      document.getElementById('val-instance-id').innerText = data.instance?.id || 'Unavailable';
      document.getElementById('val-hostname').innerText = data.hostname || 'Unavailable';
      document.getElementById('val-machine-type').innerText = data.instance?.machine_type || 'Unavailable';
      document.getElementById('val-region').innerText = data.instance?.region || 'Unavailable';
      document.getElementById('val-zone').innerText = data.instance?.zone || 'Unavailable';
      document.getElementById('val-internal-ip').innerText = data.instance?.internal_ip || 'Unavailable';
      document.getElementById('val-external-ip').innerText = data.instance?.external_ip || 'Unavailable';
      document.getElementById('val-build-id').innerText = data.build_id || 'Unavailable';
      document.getElementById('val-version').innerText = data.version || 'Unavailable';
      document.getElementById('val-environment').innerText = data.environment || 'Unavailable';
      document.getElementById('val-sa').innerText = data.service_account || 'Unavailable';

      const tagsContainer = document.getElementById('val-tags');
      tagsContainer.innerHTML = '';
      if (data.instance?.tags && data.instance.tags.length > 0) {
        data.instance.tags.forEach(t => {
          const span = document.createElement('span');
          span.className = 'tag-chip';
          span.innerText = t;
          tagsContainer.appendChild(span);
        });
      } else {
        tagsContainer.innerHTML = '<span class="data-value">None</span>';
      }
    }

    async function fetchLiveMetadata() {
      await callEndpoint(currentEndpoint, null);
    }

    document.getElementById('auto-refresh-toggle').addEventListener('change', function(e) {
      if (e.target.checked) {
        pollInterval = setInterval(fetchLiveMetadata, 2000);
      } else {
        clearInterval(pollInterval);
        pollInterval = null;
      }
    });

    function copyRawJSON() {
      const text = document.getElementById('json-code-block').innerText;
      navigator.clipboard.writeText(text).then(() => {
        alert('JSON payload copied to clipboard!');
      });
    }
  </script>
</body>
</html>
`
