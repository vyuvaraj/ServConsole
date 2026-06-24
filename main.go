package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/glebarez/go-sqlite"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	_ "github.com/sijms/go-ora/v2"

	"github.com/vyuvaraj/ServShared"
)

var (
	port       = flag.Int("port", 8083, "Port to listen on")
	gateUrl    = flag.String("gate-url", "http://localhost:8080", "ServGate base URL")
	storeUrl   = flag.String("store-url", "http://localhost:8081", "ServStore base URL")
	queueUrl   = flag.String("queue-url", "http://localhost:8082", "ServQueue base URL")
	traceUrl   = flag.String("trace-url", "http://localhost:8090", "ServTrace base URL")
	tunnelUrl  = flag.String("tunnel-url", "http://localhost:8443", "ServTunnel base URL")
	authToken  = flag.String("auth-token", "gateway-secret-token", "Default API Auth token to use for downstream proxying")
	gateConfig = flag.String("gate-config", "../ServGate/config.json", "Path to ServGate config.json")
)

// ServDiscovery is the structure of the SERVVERSE_DISCOVERY JSON manifest.
// Set the env var SERVVERSE_DISCOVERY to a JSON string or a file path to
// override any of these values without recompiling or changing CLI flags.
type ServDiscovery struct {
	Gate         string `json:"gate"`          // ServGate base URL
	Store        string `json:"store"`         // ServStore base URL
	Queue        string `json:"queue"`         // ServQueue base URL
	Trace        string `json:"trace"`         // ServTrace base URL
	Tunnel       string `json:"tunnel"`        // ServTunnel base URL
	ConsolePort  int    `json:"console_port"` // Override listen port
	JWTSecret    string `json:"jwt_secret"`   // Shared JWT signing secret
	OTLPEndpoint string `json:"otlp_endpoint"` // Shared OpenTelemetry collector
	GateConfig   string `json:"gate_config"`  // Path to ServGate config.json
	AuthToken    string `json:"auth_token"`   // Downstream proxy auth token
}

// activeDiscovery holds the resolved service discovery config after startup.
var activeDiscovery ServDiscovery

// loadDiscovery resolves service endpoints from the SERVVERSE_DISCOVERY env var
// (JSON string or file path), falling back to CLI flag values.
func loadDiscovery() ServDiscovery {
	d := ServDiscovery{
		Gate:         *gateUrl,
		Store:        *storeUrl,
		Queue:        *queueUrl,
		Trace:        *traceUrl,
		Tunnel:       *tunnelUrl,
		ConsolePort:  *port,
		AuthToken:    *authToken,
		GateConfig:   *gateConfig,
		OTLPEndpoint: os.Getenv("SERV_OTLP_ENDPOINT"),
		JWTSecret:    os.Getenv("SERV_JWT_SECRET"),
	}

	raw := os.Getenv("SERVVERSE_DISCOVERY")
	if raw == "" {
		log.Println("[discovery] SERVVERSE_DISCOVERY not set — using CLI flags / defaults")
		return d
	}

	// Try as JSON string first, then as a file path
	var manifest ServDiscovery
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		// Treat as a file path
		data, ferr := os.ReadFile(raw)
		if ferr != nil {
			log.Printf("[discovery] SERVVERSE_DISCOVERY is neither valid JSON nor a readable file: %v", ferr)
			return d
		}
		if err2 := json.Unmarshal(data, &manifest); err2 != nil {
			log.Printf("[discovery] Failed to parse discovery file %s: %v", raw, err2)
			return d
		}
		log.Printf("[discovery] Loaded from file: %s", raw)
	} else {
		log.Println("[discovery] Loaded from SERVVERSE_DISCOVERY env var (inline JSON)")
	}

	// Merge: only override fields that are non-empty in the manifest
	if manifest.Gate != "" { d.Gate = manifest.Gate }
	if manifest.Store != "" { d.Store = manifest.Store }
	if manifest.Queue != "" { d.Queue = manifest.Queue }
	if manifest.ConsolePort != 0 { d.ConsolePort = manifest.ConsolePort }
	if manifest.AuthToken != "" { d.AuthToken = manifest.AuthToken }
	if manifest.JWTSecret != "" { d.JWTSecret = manifest.JWTSecret }
	if manifest.OTLPEndpoint != "" { d.OTLPEndpoint = manifest.OTLPEndpoint }
	if manifest.GateConfig != "" { d.GateConfig = manifest.GateConfig }
	if manifest.Trace != "" { d.Trace = manifest.Trace }
	if manifest.Tunnel != "" { d.Tunnel = manifest.Tunnel }

	return d
}

type Route struct {
	Prefix             string   `json:"prefix"`
	Target             string   `json:"target"`
	Targets            []string `json:"targets,omitempty"`
	LoadBalancer       string   `json:"load_balancer,omitempty"`
	TranspileType      string   `json:"transpile_type,omitempty"`
	Middleware         string   `json:"middleware,omitempty"`
	ResponseMiddleware string   `json:"response_middleware,omitempty"`
	RateLimitRPM       int      `json:"rate_limit_rpm,omitempty"`
	PromptGuard        bool     `json:"prompt_guard,omitempty"`
	PiiRedact          bool     `json:"pii_redact,omitempty"`
	SemanticCache      bool     `json:"semantic_cache,omitempty"`
}

type GatewayConfig struct {
	Addr      string  `json:"addr"`
	AuthToken string  `json:"auth_token"`
	TlsCert   string  `json:"tls_cert"`
	TlsKey    string  `json:"tls_key"`
	Routes    []Route `json:"routes"`
	Signature string  `json:"signature,omitempty"`
}

type ComponentStatus struct {
	Name      string    `json:"name"`
	Online    bool      `json:"online"`
	Url       string    `json:"url"`
	LatencyMs int64     `json:"latency_ms,omitempty"`
	Details   any       `json:"details,omitempty"`
}

var configMu sync.Mutex

func main() {
	flag.Parse()

	// Load service discovery (SERVVERSE_DISCOVERY env var > CLI flags > defaults)
	activeDiscovery = loadDiscovery()

	// Apply resolved URLs back to the flag vars so all handlers pick them up
	*gateUrl    = activeDiscovery.Gate
	*storeUrl   = activeDiscovery.Store
	*queueUrl   = activeDiscovery.Queue
	*traceUrl   = activeDiscovery.Trace
	*tunnelUrl  = activeDiscovery.Tunnel
	*port       = activeDiscovery.ConsolePort
	*authToken  = activeDiscovery.AuthToken
	*gateConfig = activeDiscovery.GateConfig

	log.Printf("[discovery] ServGate  → %s", *gateUrl)
	log.Printf("[discovery] ServStore → %s", *storeUrl)
	log.Printf("[discovery] ServQueue → %s", *queueUrl)
	log.Printf("[discovery] ServTrace → %s", *traceUrl)
	log.Printf("[discovery] ServTunnel → %s", *tunnelUrl)
	if activeDiscovery.OTLPEndpoint != "" {
		log.Printf("[discovery] OTLP      → %s", activeDiscovery.OTLPEndpoint)
	}

	// Initialize OIDC and Auth Secret
	initJWTSecret()
	initOIDC()
	loadAuditLogs()
	loadMigrations()

	// Parse downstream URLs
	gURL, err := url.Parse(*gateUrl)
	if err != nil {
		log.Fatalf("Invalid gate-url: %v", err)
	}
	sURL, err := url.Parse(*storeUrl)
	if err != nil {
		log.Fatalf("Invalid store-url: %v", err)
	}
	qURL, err := url.Parse(*queueUrl)
	if err != nil {
		log.Fatalf("Invalid queue-url: %v", err)
	}
	tURL, err := url.Parse(*traceUrl)
	if err != nil {
		log.Fatalf("Invalid trace-url: %v", err)
	}
	tunURL, err := url.Parse(*tunnelUrl)
	if err != nil {
		log.Fatalf("Invalid tunnel-url: %v", err)
	}

	// Create reverse proxies
	gateProxy := httputil.NewSingleHostReverseProxy(gURL)
	storeProxy := httputil.NewSingleHostReverseProxy(sURL)
	queueProxy := httputil.NewSingleHostReverseProxy(qURL)
	traceProxy := httputil.NewSingleHostReverseProxy(tURL)
	tunnelProxy := httputil.NewSingleHostReverseProxy(tunURL)

	// Adjust Director to rewrite request path and set Authorization headers
	configureProxyDirector(gateProxy, gURL, "/api/proxy/gate", *authToken)
	configureProxyDirector(storeProxy, sURL, "/api/proxy/store", "")
	configureProxyDirector(queueProxy, qURL, "/api/proxy/queue", "secret-token")
	configureProxyDirector(traceProxy, tURL, "/api/proxy/trace", "")
	configureProxyDirector(tunnelProxy, tunURL, "/api/proxy/tunnel", "")

	mux := http.NewServeMux()

	// Health probes
	mux.HandleFunc("/healthz", ServShared.HealthzHandler)
	mux.HandleFunc("/readyz", ServShared.ReadyzHandler)

	// 1. ServConsole Status Aggregator & Routes API
	mux.HandleFunc("/api/status", authorizeConsole(handleStatus))
	mux.HandleFunc("/api/events", authorizeConsole(handleEvents))
	mux.HandleFunc("/api/routes", authorizeConsole(handleRoutes))
	mux.HandleFunc("/api/cluster", authorizeConsole(handleCluster))
	mux.HandleFunc("/api/cluster/rebalance", authorizeConsole(handleRebalance))
	mux.HandleFunc("/api/discovery", handleDiscovery)
	mux.HandleFunc("/api/audit-logs", authorizeConsole(handleGetAuditLogs))
	mux.HandleFunc("/api/db/query", authorizeConsole(handleDbQuery))
	mux.HandleFunc("/api/db/migrations", authorizeConsole(handleMigrations))
	mux.HandleFunc("/api/topology", authorizeConsole(handleTopology))

	// 2. Auth E&OIDC
	mux.HandleFunc("/api/auth/config", handleAuthConfig)
	mux.HandleFunc("/api/auth/login", handleLogin)
	mux.HandleFunc("/api/auth/callback", handleCallback)
	mux.HandleFunc("/api/auth/logout", handleLogout)
	mux.HandleFunc("/api/auth/me", authorizeConsole(handleAuthMe))

	// 3. Proxies
	mux.Handle("/api/proxy/gate/", authorizeConsole(checkProxyRBAC(gateProxy.ServeHTTP)))
	mux.Handle("/api/proxy/store/", authorizeConsole(checkProxyRBAC(storeProxy.ServeHTTP)))
	mux.Handle("/api/proxy/queue/", authorizeConsole(checkProxyRBAC(queueProxy.ServeHTTP)))
	mux.Handle("/api/proxy/trace/", authorizeConsole(traceProxy.ServeHTTP))
	mux.Handle("/api/proxy/tunnel/", authorizeConsole(tunnelProxy.ServeHTTP))

	fileServer := http.FileServer(http.Dir("./web"))
	mux.Handle("/", fileServer)

	var handler http.Handler = mux
	handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/") {
			r.URL.Path = "/api/" + strings.TrimPrefix(r.URL.Path, "/api/v1/")
		}
		mux.ServeHTTP(w, r)
	})

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("Starting ServConsole on http://localhost%s...", addr)
	log.Printf("Proxying Gateways to %s", *gateUrl)
	log.Printf("Proxying Storage to %s", *storeUrl)
	log.Printf("Proxying Queues to %s", *queueUrl)

	server := &http.Server{
		Addr:    addr,
		Handler: handler,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go startEventBroadcaster(ctx)


	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Failed to start server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("Console: Shutting down gracefully...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("Console: Server forced to shutdown: %v", err)
	} else {
		log.Println("Console: Server exited cleanly")
	}
}

func configureProxyDirector(proxy *httputil.ReverseProxy, target *url.URL, prefix string, defaultToken string) {
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		// Rewrite Path: remove prefix
		req.URL.Path = strings.TrimPrefix(req.URL.Path, prefix)
		if req.URL.Path == "" {
			req.URL.Path = "/"
		}
		// Set Target Host
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.Host = target.Host

		// Set Auth Header if not present
		if defaultToken != "" && req.Header.Get("Authorization") == "" {
			req.Header.Set("Authorization", "Bearer "+defaultToken)
		}
	}

	proxy.ModifyResponse = func(resp *http.Response) error {
		req := resp.Request
		if req != nil && (req.Method == "POST" || req.Method == "PUT" || req.Method == "DELETE") {
			user := req.Header.Get("X-Console-User")
			action := getProxyActionName(prefix, req.URL.Path)
			addAuditLog(user, action, req.Method, req.URL.Path, resp.StatusCode)
		}
		return nil
	}
}

type APIError struct {
	Error   string `json:"error"`
	Code    string `json:"code"`
	TraceID string `json:"trace_id,omitempty"`
}

func WriteJSONError(w http.ResponseWriter, r *http.Request, msg string, code string, status int) {
	traceID := ""
	if r != nil {
		traceparent := r.Header.Get("traceparent")
		if traceparent != "" {
			parts := strings.Split(traceparent, "-")
			if len(parts) >= 2 {
				traceID = parts[1]
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(APIError{
		Error:   msg,
		Code:    code,
		TraceID: traceID,
	})
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	statuses := []ComponentStatus{
		checkStatus("ServGate", *gateUrl),
		checkStatus("ServStore", *storeUrl),
		checkStatus("ServQueue", *queueUrl),
		checkStatus("ServTunnel", *tunnelUrl),
	}

	json.NewEncoder(w).Encode(map[string]any{
		"timestamp":  time.Now().Format(time.RFC3339),
		"components": statuses,
	})
}

func checkStatus(name string, baseUrl string) ComponentStatus {
	client := http.Client{
		Timeout: 1 * time.Second,
	}

	// 1. Poll standardized /healthz endpoint
	reqUrl := fmt.Sprintf("%s/healthz", strings.TrimSuffix(baseUrl, "/"))
	req, err := http.NewRequest("GET", reqUrl, nil)
	if err != nil {
		return ComponentStatus{Name: name, Online: false, Url: baseUrl}
	}

	// Propagate default credentials for internal check
	switch name {
	case "ServGate":
		req.Header.Set("Authorization", "Bearer "+*authToken)
	case "ServQueue":
		req.Header.Set("Authorization", "Bearer secret-token")
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return ComponentStatus{Name: name, Online: false, Url: baseUrl}
	}
	resp.Body.Close()

	latency := time.Since(start).Milliseconds()

	if resp.StatusCode != http.StatusOK {
		return ComponentStatus{
			Name:      name,
			Online:    false,
			Url:       baseUrl,
			LatencyMs: latency,
		}
	}

	// 2. Fetch metrics for details if healthy
	var details any
	var detailsPath string
	switch name {
	case "ServStore":
		detailsPath = "/console/metrics"
	case "ServQueue":
		detailsPath = "/api/stats"
	case "ServGate":
		detailsPath = "/"
	}

	if detailsPath != "" {
		detUrl := fmt.Sprintf("%s%s", strings.TrimSuffix(baseUrl, "/"), detailsPath)
		dreq, derr := http.NewRequest("GET", detUrl, nil)
		if derr == nil {
			switch name {
			case "ServGate":
				dreq.Header.Set("Authorization", "Bearer "+*authToken)
			case "ServQueue":
				dreq.Header.Set("Authorization", "Bearer secret-token")
			}
			dresp, derr2 := client.Do(dreq)
			if derr2 == nil {
				bodyBytes, _ := io.ReadAll(dresp.Body)
				dresp.Body.Close()
				if len(bodyBytes) > 0 {
					_ = json.Unmarshal(bodyBytes, &details)
				}
			}
		}
	}

	return ComponentStatus{
		Name:      name,
		Online:    true,
		Url:       baseUrl,
		LatencyMs: latency,
		Details:   details,
	}
}

func handleRoutes(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost || r.Method == http.MethodDelete {
		if getUserRole(r) != "admin" {
			WriteJSONError(w, r, "Forbidden: Admin role required to modify routes", "ERR_FORBIDDEN", http.StatusForbidden)
			return
		}
	}

	configMu.Lock()
	defer configMu.Unlock()

	var prov ConfigProvider
	if os.Getenv("SERV_CONFIG_S3_BUCKET") != "" || os.Getenv("SERVVERSE_DISCOVERY") != "" {
		prov = NewS3ConfigProvider()
	} else {
		prov = NewLocalFileProvider(*gateConfig)
	}

	cfg, err := prov.Load()
	if err != nil {
		if os.IsNotExist(err) {
			cfg = &GatewayConfig{
				Addr:      ":8080",
				AuthToken: *authToken,
				Routes:    []Route{},
			}
		} else {
			WriteJSONError(w, r, "Failed to read config: "+err.Error(), "ERR_CONFIG_LOAD_FAILED", http.StatusInternalServerError)
			return
		}
	}

	if r.Method == http.MethodPost {
		var newRoute Route
		if err := json.NewDecoder(r.Body).Decode(&newRoute); err != nil {
			WriteJSONError(w, r, "Invalid route payload", "ERR_INVALID_ROUTE_PAYLOAD", http.StatusBadRequest)
			return
		}

		found := false
		for i, rt := range cfg.Routes {
			if rt.Prefix == newRoute.Prefix {
				cfg.Routes[i] = newRoute
				found = true
				break
			}
		}
		if !found {
			cfg.Routes = append(cfg.Routes, newRoute)
		}

		if err := prov.Save(cfg); err != nil {
			WriteJSONError(w, r, "Failed to save config: "+err.Error(), "ERR_CONFIG_SAVE_FAILED", http.StatusInternalServerError)
			return
		}

		user := r.Header.Get("X-Console-User")
		addAuditLog(user, "Register/Update API Route: "+newRoute.Prefix, r.Method, r.URL.Path, http.StatusOK)
		log.Printf("Successfully updated config with route prefix: %s", newRoute.Prefix)
	}

	if r.Method == http.MethodDelete {
		prefix := r.URL.Query().Get("prefix")
		if prefix == "" {
			WriteJSONError(w, r, "Missing prefix query parameter", "ERR_INVALID_ROUTE_PAYLOAD", http.StatusBadRequest)
			return
		}

		newRoutes := []Route{}
		found := false
		for _, rt := range cfg.Routes {
			if rt.Prefix == prefix {
				found = true
			} else {
				newRoutes = append(newRoutes, rt)
			}
		}

		if !found {
			WriteJSONError(w, r, "Route not found", "ERR_ROUTE_NOT_FOUND", http.StatusNotFound)
			return
		}

		cfg.Routes = newRoutes
		if err := prov.Save(cfg); err != nil {
			WriteJSONError(w, r, "Failed to save config: "+err.Error(), "ERR_CONFIG_SAVE_FAILED", http.StatusInternalServerError)
			return
		}

		// Forward to ServGate
		gateDeleteUrl := fmt.Sprintf("%s/api/routes?prefix=%s", strings.TrimSuffix(*gateUrl, "/"), url.QueryEscape(prefix))
		greq, gerr := http.NewRequest(http.MethodDelete, gateDeleteUrl, nil)
		if greq != nil && gerr == nil {
			if *authToken != "" {
				greq.Header.Set("Authorization", "Bearer "+*authToken)
			}
			gclient := &http.Client{Timeout: 3 * time.Second}
			gresp, gerr2 := gclient.Do(greq)
			if gerr2 == nil {
				gresp.Body.Close()
			}
		}

		user := r.Header.Get("X-Console-User")
		addAuditLog(user, "Delete API Route: "+prefix, r.Method, r.URL.Path, http.StatusOK)
		log.Printf("Successfully deleted route prefix: %s", prefix)
	}

	w.Header().Set("Content-Type", "application/json")
	if cfg.Routes == nil {
		cfg.Routes = []Route{}
	}
	json.NewEncoder(w).Encode(cfg.Routes)
}

// NodeHealth wraps cluster NodeInfo with derived replication-lag metrics.
type NodeHealth struct {
	NodeID        string `json:"node_id"`
	Address       string `json:"address"`
	Status        string `json:"status"`
	Region        string `json:"region"`
	LastSeenAgoMs int64  `json:"last_seen_ago_ms"`
	LagStatus     string `json:"lag_status"` // "healthy" | "warning" | "critical"
	Load          int64  `json:"load"`
}

type ClusterHealth struct {
	Nodes          []NodeHealth `json:"nodes"`
	OnlineCount    int          `json:"online_count"`
	OfflineCount   int          `json:"offline_count"`
	ErasureCoding  bool         `json:"erasure_coding"`
	DataShards     int          `json:"data_shards"`
	ParityShards   int          `json:"parity_shards"`
	ClusterHealthy bool         `json:"cluster_healthy"`
}

func handleCluster(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	client := http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequest("GET", strings.TrimSuffix(*storeUrl, "/")+"/console/cluster/status", nil)
	if err != nil {
		json.NewEncoder(w).Encode(ClusterHealth{})
		return
	}

	type rawNode struct {
		NodeID   string    `json:"node_id"`
		Address  string    `json:"address"`
		Status   string    `json:"status"`
		LastSeen time.Time `json:"last_seen"`
		Load     int64     `json:"load"`
		Region   string    `json:"region"`
	}

	resp, err := client.Do(req)
	if err != nil {
		json.NewEncoder(w).Encode(ClusterHealth{})
		return
	}
	defer resp.Body.Close()

	var rawNodes []rawNode
	if err := json.NewDecoder(resp.Body).Decode(&rawNodes); err != nil {
		json.NewEncoder(w).Encode(ClusterHealth{})
		return
	}

	now := time.Now()
	var nodes []NodeHealth
	online, offline := 0, 0

	for _, n := range rawNodes {
		lagMs := int64(0)
		lagStatus := "healthy"
		if !n.LastSeen.IsZero() {
			lagMs = now.Sub(n.LastSeen).Milliseconds()
			switch {
			case lagMs > 10000:
				lagStatus = "critical"
			case lagMs > 5000:
				lagStatus = "warning"
			}
		}
		if n.Status == "online" {
			online++
		} else {
			offline++
			lagStatus = "critical"
		}
		nodes = append(nodes, NodeHealth{
			NodeID:        n.NodeID,
			Address:       n.Address,
			Status:        n.Status,
			Region:        n.Region,
			LastSeenAgoMs: lagMs,
			LagStatus:     lagStatus,
			Load:          n.Load,
		})
	}

	json.NewEncoder(w).Encode(ClusterHealth{
		Nodes:          nodes,
		OnlineCount:    online,
		OfflineCount:   offline,
		ErasureCoding:  false,
		DataShards:     2,
		ParityShards:   1,
		ClusterHealthy: offline == 0 && len(nodes) > 0,
	})
}

func handleRebalance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	role := getUserRole(r)
	if role != "admin" && role != "operator" {
		http.Error(w, "Forbidden: Admin or Operator role required to trigger cluster rebalance", http.StatusForbidden)
		return
	}
	client := http.Client{Timeout: 3 * time.Second}
	body := strings.NewReader(`{"source_node":{"node_id":"servconsole","address":"localhost:8083","status":"online"},"peers":{}}`)
	req, err := http.NewRequest("POST",
		strings.TrimSuffix(*storeUrl, "/")+"/console/cluster/gossip", body)
	if err != nil {
		http.Error(w, "Request build failed", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "ServStore unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	user := r.Header.Get("X-Console-User")
	addAuditLog(user, "Trigger Cluster Rebalance", r.Method, r.URL.Path, resp.StatusCode)
	log.Printf("Rebalance gossip round triggered, ServStore responded: %d", resp.StatusCode)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "rebalance_triggered"})
}

// handleDiscovery returns the currently active service discovery configuration.
// Sensitive fields (jwt_secret, auth_token) are redacted before sending to the browser.
func handleDiscovery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Safe view — redact secrets
	safe := map[string]interface{}{
		"gate":          activeDiscovery.Gate,
		"store":         activeDiscovery.Store,
		"queue":         activeDiscovery.Queue,
		"console_port":  activeDiscovery.ConsolePort,
		"otlp_endpoint": activeDiscovery.OTLPEndpoint,
		"gate_config":   activeDiscovery.GateConfig,
		"jwt_secret":    redact(activeDiscovery.JWTSecret),
		"auth_token":    redact(activeDiscovery.AuthToken),
		"source":        discoverySource(),
	}
	json.NewEncoder(w).Encode(safe)
}

func redact(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 4 {
		return "****"
	}
	return s[:2] + strings.Repeat("*", len(s)-4) + s[len(s)-2:]
}

func discoverySource() string {
	if os.Getenv("SERVVERSE_DISCOVERY") != "" {
		return "SERVVERSE_DISCOVERY"
	}
	return "cli-flags/defaults"
}

func authorizeConsole(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jwtSec := os.Getenv("SERV_JWT_SECRET")
		if jwtSec == "" && oidcIssuer == "" {
			next(w, r)
			return
		}

		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			if cookie, err := r.Cookie("token"); err == nil {
				authHeader = "Bearer " + cookie.Value
			}
		}

		if authHeader == "" {
			http.Error(w, "Unauthorized: missing token", http.StatusUnauthorized)
			return
		}

		token := strings.TrimPrefix(authHeader, "Bearer ")
		username, role, ok := validateJWT(token, jwtSecBytes)
		if !ok {
			http.Error(w, "Unauthorized: invalid token", http.StatusUnauthorized)
			return
		}

		r.Header.Set("X-Console-User", username)
		r.Header.Set("X-Console-Role", role)
		next(w, r)
	}
}

func validateJWT(tokenStr string, secret []byte) (string, string, bool) {
	parts := strings.Split(tokenStr, ".")
	if len(parts) != 3 {
		return "", "", false
	}

	headerPart, payloadPart, signaturePart := parts[0], parts[1], parts[2]
	
	// Validate signature
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(headerPart + "." + payloadPart))
	expectedMac := mac.Sum(nil)
	
	// Base64Url decode signaturePart
	sigBytes, err := base64UrlDecode(signaturePart)
	if err != nil || !hmac.Equal(sigBytes, expectedMac) {
		return "", "", false
	}

	// Base64Url decode payloadPart and extract username, exp
	payloadBytes, err := base64UrlDecode(payloadPart)
	if err != nil {
		return "", "", false
	}

	var claims struct {
		Username string `json:"username"`
		Role     string `json:"role"`
		Exp      int64  `json:"exp"`
	}
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return "", "", false
	}

	// Check expiration
	if claims.Exp > 0 && time.Now().Unix() > claims.Exp {
		return "", "", false
	}

	role := claims.Role
	if role == "" {
		if claims.Username == "admin" {
			role = "admin"
		} else if claims.Username == "operator" || claims.Username == "developer-bob" {
			role = "operator"
		} else {
			role = "viewer"
		}
	}

	return claims.Username, role, true
}

func base64UrlDecode(s string) ([]byte, error) {
	if l := len(s) % 4; l > 0 {
		s += strings.Repeat("=", 4-l)
	}
	s = strings.ReplaceAll(s, "-", "+")
	s = strings.ReplaceAll(s, "_", "/")
	return base64.URLEncoding.DecodeString(s)
}

// --- OIDC SSO and Audit Logs Implementation ---

var (
	oidcIssuer       = os.Getenv("SERV_OIDC_ISSUER")
	oidcClientID     = os.Getenv("SERV_OIDC_CLIENT_ID")
	oidcClientSecret = os.Getenv("SERV_OIDC_CLIENT_SECRET")
	oidcRedirectURL  = os.Getenv("SERV_OIDC_REDIRECT_URL")

	oidcAuthURL  string
	oidcTokenURL string
	jwtSecBytes  []byte
)

func initJWTSecret() {
	// Execute service discovery load so variables are populated
	activeDiscovery = loadDiscovery()
	sec := activeDiscovery.JWTSecret
	if sec == "" {
		sec = fmt.Sprintf("ephemeral-%d-%d", time.Now().UnixNano(), sha256.Sum256([]byte("servconsole-salt")))
		log.Println("[auth] SERV_JWT_SECRET not set. Generated ephemeral session key.")
	}
	jwtSecBytes = []byte(sec)
}

func initOIDC() {
	if oidcIssuer == "" {
		return
	}
	log.Printf("[OIDC] Issuer configured: %s", oidcIssuer)
	wellKnown := strings.TrimSuffix(oidcIssuer, "/") + "/.well-known/openid-configuration"
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(wellKnown)
	if err == nil {
		defer resp.Body.Close()
		var config struct {
			AuthorizationEndpoint string `json:"authorization_endpoint"`
			TokenEndpoint         string `json:"token_endpoint"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&config); err == nil {
			oidcAuthURL = config.AuthorizationEndpoint
			oidcTokenURL = config.TokenEndpoint
			log.Printf("[OIDC] Discovered endpoints: auth=%s, token=%s", oidcAuthURL, oidcTokenURL)
			return
		}
	}
	
	oidcAuthURL = strings.TrimSuffix(oidcIssuer, "/") + "/protocol/openid-connect/auth"
	oidcTokenURL = strings.TrimSuffix(oidcIssuer, "/") + "/protocol/openid-connect/token"
	log.Printf("[OIDC] Discovery failed or skipped. Using default endpoints: auth=%s, token=%s", oidcAuthURL, oidcTokenURL)
}

func generateLocalJWT(username string) (string, error) {
	role := "viewer"
	if username == "admin" {
		role = "admin"
	} else if username == "operator" || username == "developer-bob" {
		role = "operator"
	}
	header := base64UrlEncode([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := base64UrlEncode([]byte(fmt.Sprintf(`{"username":%q,"exp":%d,"role":%q}`, username, time.Now().Add(24*time.Hour).Unix(), role)))
	
	mac := hmac.New(sha256.New, jwtSecBytes)
	mac.Write([]byte(header + "." + payload))
	signature := base64UrlEncode(mac.Sum(nil))
	
	return header + "." + payload + "." + signature, nil
}

func base64UrlEncode(b []byte) string {
	s := base64.URLEncoding.EncodeToString(b)
	return strings.TrimRight(s, "=")
}

func handleAuthConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"sso_enabled": oidcIssuer != "",
		"issuer":      oidcIssuer,
		"client_id":   oidcClientID,
	})
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if oidcIssuer == "" {
		http.Error(w, "OIDC SSO is not configured", http.StatusBadRequest)
		return
	}
	
	u, err := url.Parse(oidcAuthURL)
	if err != nil {
		http.Error(w, "Invalid auth URL", http.StatusInternalServerError)
		return
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", oidcClientID)
	q.Set("redirect_uri", oidcRedirectURL)
	q.Set("scope", "openid profile email")
	q.Set("state", "random-state-string")
	u.RawQuery = q.Encode()
	
	http.Redirect(w, r, u.String(), http.StatusTemporaryRedirect)
}

func handleCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "Missing code query param", http.StatusBadRequest)
		return
	}
	
	data := url.Values{}
	data.Set("grant_type", "authorization_code")
	data.Set("code", code)
	data.Set("redirect_uri", oidcRedirectURL)
	data.Set("client_id", oidcClientID)
	data.Set("client_secret", oidcClientSecret)
	
	req, err := http.NewRequest("POST", oidcTokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		http.Error(w, "Failed to create token exchange request: "+err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	
	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "Token exchange failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		http.Error(w, fmt.Sprintf("Token exchange returned status %d: %s", resp.StatusCode, string(body)), http.StatusInternalServerError)
		return
	}
	
	var res struct {
		IDToken string `json:"id_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		http.Error(w, "Failed to decode token response: "+err.Error(), http.StatusInternalServerError)
		return
	}
	
	if res.IDToken == "" {
		http.Error(w, "No id_token returned", http.StatusInternalServerError)
		return
	}
	
	parts := strings.Split(res.IDToken, ".")
	if len(parts) != 3 {
		http.Error(w, "Invalid ID token format", http.StatusInternalServerError)
		return
	}
	payloadBytes, err := base64UrlDecode(parts[1])
	if err != nil {
		http.Error(w, "Failed to decode ID token payload: "+err.Error(), http.StatusInternalServerError)
		return
	}
	
	var claims struct {
		PreferredUsername string `json:"preferred_username"`
		Email             string `json:"email"`
		Sub               string `json:"sub"`
	}
	_ = json.Unmarshal(payloadBytes, &claims)
	
	username := claims.PreferredUsername
	if username == "" {
		username = claims.Email
	}
	if username == "" {
		username = claims.Sub
	}
	if username == "" {
		username = "sso-user"
	}
	
	localToken, err := generateLocalJWT(username)
	if err != nil {
		http.Error(w, "Failed to generate session token: "+err.Error(), http.StatusInternalServerError)
		return
	}
	
	http.SetCookie(w, &http.Cookie{
		Name:     "token",
		Value:    localToken,
		Path:     "/",
		HttpOnly: true,
		Secure:   false,
		Expires:  time.Now().Add(24 * time.Hour),
	})
	
	addAuditLog(username, "SSO Login Success", "GET", "/api/auth/callback", http.StatusOK)
	http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	user := r.Header.Get("X-Console-User")
	addAuditLog(user, "User Logged Out", "POST", "/api/auth/logout", http.StatusOK)

	http.SetCookie(w, &http.Cookie{
		Name:     "token",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	})
	
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"logged_out"}`))
}

// --- Audit Logging System ---

type AuditEntry struct {
	Timestamp time.Time `json:"timestamp"`
	User      string    `json:"user"`
	Action    string    `json:"action"`
	Method    string    `json:"method"`
	Path      string    `json:"path"`
	Status    int       `json:"status"`
}

var (
	auditLogs   []AuditEntry
	auditLogsMu sync.Mutex
	auditFile   = "audit.json"
)

func loadAuditLogs() {
	auditLogsMu.Lock()
	defer auditLogsMu.Unlock()
	
	data, err := os.ReadFile(auditFile)
	if err == nil {
		_ = json.Unmarshal(data, &auditLogs)
	}
	if auditLogs == nil {
		auditLogs = []AuditEntry{}
	}
}

func saveAuditLogs() {
	data, err := json.MarshalIndent(auditLogs, "", "  ")
	if err == nil {
		_ = os.WriteFile(auditFile, data, 0644)
	}
}

func addAuditLog(user string, action string, method string, path string, status int) {
	auditLogsMu.Lock()
	defer auditLogsMu.Unlock()
	
	if user == "" {
		user = "anonymous"
	}
	
	entry := AuditEntry{
		Timestamp: time.Now(),
		User:      user,
		Action:    action,
		Method:    method,
		Path:      path,
		Status:    status,
	}
	
	auditLogs = append([]AuditEntry{entry}, auditLogs...)
	if len(auditLogs) > 200 {
		auditLogs = auditLogs[:200]
	}
	
	saveAuditLogs()
}

func handleGetAuditLogs(w http.ResponseWriter, r *http.Request) {
	auditLogsMu.Lock()
	defer auditLogsMu.Unlock()
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(auditLogs)
}

func getProxyActionName(prefix string, path string) string {
	switch prefix {
	case "/api/proxy/gate":
		if strings.Contains(path, "/api/admin/middleware") {
			return "Update Gateway WASM Middleware"
		}
		return "Gateway API Proxy Request"
	case "/api/proxy/store":
		return "Storage S3 Proxy Request"
	case "/api/proxy/queue":
		if strings.Contains(path, "/transform") {
			return "Register Queue WASM Transform"
		} else if strings.Contains(path, "/dlq") {
			return "Configure Queue DLQ"
		} else if strings.Contains(path, "/publish") {
			return "Publish STOMP Message via API"
		}
		return "Queue Broker Proxy Request"
	default:
		return "Proxy Request"
	}
}

func handleDbQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Driver  string `json:"driver"`
		ConnStr string `json:"connStr"`
		Query   string `json:"query"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	driver := strings.ToLower(req.Driver)
	connStr := req.ConnStr
	query := req.Query

	if driver == "" || connStr == "" || query == "" {
		http.Error(w, "Missing driver, connStr, or query", http.StatusBadRequest)
		return
	}

	// Validate driver
	switch driver {
	case "sqlite", "sqlite3":
		driver = "sqlite"
		// convert sqlite:// prefix if present
		connStr = strings.TrimPrefix(connStr, "sqlite://")
	case "postgres", "postgresql":
		driver = "postgres"
	case "mysql":
		driver = "mysql"
	case "oracle":
		driver = "oracle"
	default:
		http.Error(w, "Unsupported driver: "+req.Driver, http.StatusBadRequest)
		return
	}

	// Connect to database
	db, err := sql.Open(driver, connStr)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"success": false,
			"error":   "Failed to open connection: " + err.Error(),
		})
		return
	}
	defer db.Close()

	// Test connection
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(10 * time.Second)

	startTime := time.Now()

	// Detect if it is a SELECT-like query or an Exec query
	isSelect := false
	trimmedQuery := strings.TrimSpace(strings.ToUpper(query))
	if strings.HasPrefix(trimmedQuery, "SELECT") ||
		strings.HasPrefix(trimmedQuery, "SHOW") ||
		strings.HasPrefix(trimmedQuery, "PRAGMA") ||
		strings.HasPrefix(trimmedQuery, "DESCRIBE") ||
		strings.HasPrefix(trimmedQuery, "DESC") ||
		strings.HasPrefix(trimmedQuery, "EXPLAIN") {
		isSelect = true
	}

	if isSelect {
		rows, err := db.Query(query)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"success": false,
				"error":   err.Error(),
			})
			return
		}
		defer rows.Close()

		cols, err := rows.Columns()
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"success": false,
				"error":   "Failed to get columns: " + err.Error(),
			})
			return
		}

		results := [][]any{}
		for rows.Next() {
			rowValues := make([]any, len(cols))
			rowPointers := make([]any, len(cols))
			for i := range rowValues {
				rowPointers[i] = &rowValues[i]
			}

			if err := rows.Scan(rowPointers...); err != nil {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{
					"success": false,
					"error":   "Row scanning failed: " + err.Error(),
				})
				return
			}

			// Clean up raw scanned types (e.g. []byte to string for display)
			cleanedRow := make([]any, len(cols))
			for i, v := range rowValues {
				if b, ok := v.([]byte); ok {
					cleanedRow[i] = string(b)
				} else {
					cleanedRow[i] = v
				}
			}
			results = append(results, cleanedRow)
		}

		duration := time.Since(startTime).Milliseconds()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"success":         true,
			"isSelect":        true,
			"columns":         cols,
			"rows":            results,
			"executionTimeMs": duration,
		})
	} else {
		res, err := db.Exec(query)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"success": false,
				"error":   err.Error(),
			})
			return
		}

		rowsAffected, _ := res.RowsAffected()
		lastInsertId, _ := res.LastInsertId()
		duration := time.Since(startTime).Milliseconds()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"success":         true,
			"isSelect":        false,
			"rowsAffected":    rowsAffected,
			"lastInsertId":    lastInsertId,
			"executionTimeMs": duration,
		})
	}

	user := r.Header.Get("X-Console-User")
	addAuditLog(user, fmt.Sprintf("SQL Query (%s): %.60s", driver, query), r.Method, r.URL.Path, http.StatusOK)
}

var (
	clients   = make(map[chan string]bool)
	clientsMu sync.Mutex
)

func startEventBroadcaster(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			clientsMu.Lock()
			numClients := len(clients)
			clientsMu.Unlock()
			if numClients == 0 {
				continue
			}

			statuses := []ComponentStatus{
				checkStatus("ServGate", *gateUrl),
				checkStatus("ServStore", *storeUrl),
				checkStatus("ServQueue", *queueUrl),
			}

			eventData := map[string]any{
				"timestamp":  time.Now().Format(time.RFC3339),
				"components": statuses,
			}

			bytes, err := json.Marshal(eventData)
			if err != nil {
				continue
			}

			payload := string(bytes)
			clientsMu.Lock()
			for ch := range clients {
				select {
				case ch <- payload:
				default:
				}
			}
			clientsMu.Unlock()
		}
	}
}

func handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ch := make(chan string, 10)
	clientsMu.Lock()
	clients[ch] = true
	clientsMu.Unlock()

	defer func() {
		clientsMu.Lock()
		delete(clients, ch)
		clientsMu.Unlock()
		close(ch)
	}()

	statuses := []ComponentStatus{
		checkStatus("ServGate", *gateUrl),
		checkStatus("ServStore", *storeUrl),
		checkStatus("ServQueue", *queueUrl),
	}
	initialData, _ := json.Marshal(map[string]any{
		"timestamp":  time.Now().Format(time.RFC3339),
		"components": statuses,
	})
	fmt.Fprintf(w, "data: %s\n\n", string(initialData))
	flusher.Flush()

	for {
		select {
		case msg := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// ─── Migration Auditing System ──────────────────────────────────────────────

type MigrationEntry struct {
	ID          string    `json:"id"`
	Revision    string    `json:"revision"`
	Description string    `json:"description"`
	Driver      string    `json:"driver"`
	DSN         string    `json:"dsn"`
	SQL         string    `json:"sql"`
	User        string    `json:"user"`
	Timestamp   time.Time `json:"timestamp"`
	Status      string    `json:"status"` // "success" | "failed"
	Error       string    `json:"error,omitempty"`
	Delta       string    `json:"delta"`
	DurationMs  int64     `json:"duration_ms"`
}

var (
	migrations   []MigrationEntry
	migrationsMu sync.Mutex
	migrationsFile = "migrations.json"
)

func loadMigrations() {
	migrationsMu.Lock()
	defer migrationsMu.Unlock()

	data, err := os.ReadFile(migrationsFile)
	if err == nil {
		_ = json.Unmarshal(data, &migrations)
	}
	if migrations == nil {
		migrations = []MigrationEntry{}
	}
	log.Printf("[migrations] Loaded %d migration audit entries", len(migrations))
}

func saveMigrations() {
	data, err := json.MarshalIndent(migrations, "", "  ")
	if err == nil {
		_ = os.WriteFile(migrationsFile, data, 0644)
	}
}

func extractSchemaDelta(sqlScript string) string {
	var deltas []string
	upper := strings.ToUpper(sqlScript)
	lines := strings.Split(upper, ";")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "CREATE TABLE"):
			deltas = append(deltas, "+ CREATE TABLE")
		case strings.HasPrefix(line, "ALTER TABLE"):
			if strings.Contains(line, "ADD") {
				deltas = append(deltas, "~ ALTER TABLE (ADD column)")
			} else if strings.Contains(line, "DROP") {
				deltas = append(deltas, "~ ALTER TABLE (DROP column)")
			} else if strings.Contains(line, "MODIFY") || strings.Contains(line, "ALTER COLUMN") {
				deltas = append(deltas, "~ ALTER TABLE (MODIFY column)")
			} else if strings.Contains(line, "RENAME") {
				deltas = append(deltas, "~ ALTER TABLE (RENAME)")
			} else {
				deltas = append(deltas, "~ ALTER TABLE")
			}
		case strings.HasPrefix(line, "DROP TABLE"):
			deltas = append(deltas, "- DROP TABLE")
		case strings.HasPrefix(line, "CREATE INDEX") || strings.HasPrefix(line, "CREATE UNIQUE INDEX"):
			deltas = append(deltas, "+ CREATE INDEX")
		case strings.HasPrefix(line, "DROP INDEX"):
			deltas = append(deltas, "- DROP INDEX")
		case strings.HasPrefix(line, "INSERT"):
			deltas = append(deltas, "+ INSERT (seed data)")
		case strings.HasPrefix(line, "UPDATE"):
			deltas = append(deltas, "~ UPDATE (data migration)")
		case strings.HasPrefix(line, "DELETE"):
			deltas = append(deltas, "- DELETE (data cleanup)")
		}
	}
	if len(deltas) == 0 {
		return "SQL script executed"
	}
	return strings.Join(deltas, "; ")
}

func handleMigrations(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		handleGetMigrations(w, r)
	case http.MethodPost:
		handleApplyMigration(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func handleGetMigrations(w http.ResponseWriter, _ *http.Request) {
	migrationsMu.Lock()
	defer migrationsMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(migrations)
}

func handleApplyMigration(w http.ResponseWriter, r *http.Request) {
	if getUserRole(r) != "admin" {
		WriteJSONError(w, r, "Forbidden: Admin role required to apply migrations", "ERR_FORBIDDEN", http.StatusForbidden)
		return
	}

	var req struct {
		Driver      string `json:"driver"`
		DSN         string `json:"dsn"`
		Revision    string `json:"revision"`
		Description string `json:"description"`
		SQL         string `json:"sql"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteJSONError(w, r, "Invalid request body", "ERR_INVALID_BODY", http.StatusBadRequest)
		return
	}

	if req.Driver == "" || req.DSN == "" || req.Revision == "" || req.SQL == "" {
		WriteJSONError(w, r, "Missing required fields: driver, dsn, revision, sql", "ERR_MISSING_FIELDS", http.StatusBadRequest)
		return
	}

	// Normalize driver
	driver := strings.ToLower(req.Driver)
	dsn := req.DSN
	switch driver {
	case "sqlite", "sqlite3":
		driver = "sqlite"
		dsn = strings.TrimPrefix(dsn, "sqlite://")
	case "postgres", "postgresql":
		driver = "postgres"
	case "mysql":
		driver = "mysql"
	case "oracle":
		driver = "oracle"
	default:
		WriteJSONError(w, r, "Unsupported driver: "+req.Driver, "ERR_UNSUPPORTED_DRIVER", http.StatusBadRequest)
		return
	}

	user := r.Header.Get("X-Console-User")
	if user == "" {
		user = "anonymous"
	}

	// Generate ID
	migrationID := fmt.Sprintf("mig_%d", time.Now().UnixNano())

	entry := MigrationEntry{
		ID:          migrationID,
		Revision:    req.Revision,
		Description: req.Description,
		Driver:      driver,
		DSN:         redact(dsn),
		SQL:         req.SQL,
		User:        user,
		Timestamp:   time.Now(),
	}

	// Execute the migration
	startTime := time.Now()
	db, err := sql.Open(driver, dsn)
	if err != nil {
		entry.Status = "failed"
		entry.Error = "Connection failed: " + err.Error()
		entry.DurationMs = time.Since(startTime).Milliseconds()
		entry.Delta = "—"
		persistMigration(entry, user)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(entry)
		return
	}
	defer db.Close()

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(15 * time.Second)

	_, err = db.Exec(req.SQL)
	entry.DurationMs = time.Since(startTime).Milliseconds()

	if err != nil {
		entry.Status = "failed"
		entry.Error = err.Error()
		entry.Delta = "—"
	} else {
		entry.Status = "success"
		entry.Delta = extractSchemaDelta(req.SQL)
	}

	persistMigration(entry, user)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entry)
}

func persistMigration(entry MigrationEntry, user string) {
	migrationsMu.Lock()
	defer migrationsMu.Unlock()

	migrations = append([]MigrationEntry{entry}, migrations...)
	if len(migrations) > 500 {
		migrations = migrations[:500]
	}
	saveMigrations()

	status := 200
	if entry.Status == "failed" {
		status = 500
	}
	addAuditLog(user, fmt.Sprintf("Migration %s: %s (rev %s)", entry.Status, entry.Description, entry.Revision), "POST", "/api/db/migrations", status)
}

type TopologyNode struct {
	ID        string  `json:"id"`
	Label     string  `json:"label"`
	Color     string  `json:"color"`
	Online    bool    `json:"online"`
	LatencyMs int64   `json:"latency_ms"`
	ErrorRate float64 `json:"error_rate"`
}

type TopologyEdge struct {
	From      string  `json:"from"`
	To        string  `json:"to"`
	Label     string  `json:"label"`
	LatencyMs int64   `json:"latency_ms"`
	ErrorRate float64 `json:"error_rate"`
}

type TopologyResponse struct {
	Nodes []TopologyNode `json:"nodes"`
	Edges []TopologyEdge `json:"edges"`
}

func handleTopology(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(strings.TrimSuffix(*storeUrl, "/") + "/console/traces")
	if err != nil {
		json.NewEncoder(w).Encode(TopologyResponse{Nodes: []TopologyNode{}, Edges: []TopologyEdge{}})
		return
	}
	defer resp.Body.Close()

	type rawSpan struct {
		Name         string    `json:"Name"`
		TraceID      string    `json:"TraceID"`
		SpanID       string    `json:"SpanID"`
		ParentSpanID string    `json:"ParentSpanID"`
		ServiceName  string    `json:"ServiceName"`
		DurationNs   int64     `json:"DurationNs"`
		StatusCode   string    `json:"StatusCode"`
		StartTime    time.Time `json:"StartTime"`
	}

	var spans []rawSpan
	if err := json.NewDecoder(resp.Body).Decode(&spans); err != nil {
		json.NewEncoder(w).Encode(TopologyResponse{Nodes: []TopologyNode{}, Edges: []TopologyEdge{}})
		return
	}

	nodesMap := make(map[string]*TopologyNode)
	edgesMap := make(map[string]*TopologyEdge)

	nodesMap["ServGate"] = &TopologyNode{ID: "ServGate", Label: "ServGate (Gateway)", Color: "#06b6d4", Online: true}
	nodesMap["ServStore"] = &TopologyNode{ID: "ServStore", Label: "ServStore (Storage)", Color: "#10b981", Online: true}
	nodesMap["ServQueue"] = &TopologyNode{ID: "ServQueue", Label: "ServQueue (Broker)", Color: "#f59e0b", Online: true}
	nodesMap["ServTunnel"] = &TopologyNode{ID: "ServTunnel", Label: "ServTunnel (Relay)", Color: "#6366f1", Online: true}

	spanToService := make(map[string]string)
	for _, span := range spans {
		svc := span.ServiceName
		if svc == "" {
			svc = "unknown-service"
		}
		spanToService[span.SpanID] = svc

		if _, exists := nodesMap[svc]; !exists {
			nodesMap[svc] = &TopologyNode{
				ID:    svc,
				Label: svc,
				Color: "#a855f7",
				Online: true,
			}
		}
	}

	for _, span := range spans {
		svc := span.ServiceName
		if svc == "" {
			svc = "unknown-service"
		}

		isErr := span.StatusCode == "error"
		latMs := span.DurationNs / 1e6

		nodesMap[svc].LatencyMs = (nodesMap[svc].LatencyMs + latMs) / 2
		if isErr {
			nodesMap[svc].ErrorRate = 0.1
		}

		if span.ParentSpanID != "" {
			if parentSvc, parentExists := spanToService[span.ParentSpanID]; parentExists && parentSvc != svc {
				edgeKey := fmt.Sprintf("%s->%s", parentSvc, svc)
				if _, exists := edgesMap[edgeKey]; !exists {
					edgesMap[edgeKey] = &TopologyEdge{
						From:      parentSvc,
						To:        svc,
						Label:     "Call",
						LatencyMs: latMs,
					}
				} else {
					edgesMap[edgeKey].LatencyMs = (edgesMap[edgeKey].LatencyMs + latMs) / 2
				}
				if isErr {
					edgesMap[edgeKey].ErrorRate = 0.2
				}
			}
		}

		if strings.Contains(span.Name, "PUT") || strings.Contains(span.Name, "GET") {
			edgeKey := fmt.Sprintf("%s->ServStore", svc)
			if _, exists := edgesMap[edgeKey]; !exists {
				edgesMap[edgeKey] = &TopologyEdge{
					From:      svc,
					To:        "ServStore",
					Label:     "S3",
					LatencyMs: latMs,
				}
			}
		}
		if strings.Contains(span.Name, "publish") || strings.Contains(span.Name, "subscribe") {
			edgeKey := fmt.Sprintf("%s->ServQueue", svc)
			if _, exists := edgesMap[edgeKey]; !exists {
				edgesMap[edgeKey] = &TopologyEdge{
					From:      svc,
					To:        "ServQueue",
					Label:     "STOMP",
					LatencyMs: latMs,
				}
			}
		}
	}

	var nodes []TopologyNode
	for _, n := range nodesMap {
		nodes = append(nodes, *n)
	}

	var edges []TopologyEdge
	for _, e := range edgesMap {
		edges = append(edges, *e)
	}

	for _, n := range nodes {
		if n.ID != "ServGate" && n.ID != "ServStore" && n.ID != "ServQueue" && n.ID != "ServTunnel" {
			edgeKey := fmt.Sprintf("ServGate->%s", n.ID)
			if _, exists := edgesMap[edgeKey]; !exists {
				edges = append(edges, TopologyEdge{
					From:      "ServGate",
					To:        n.ID,
					Label:     "HTTP",
					LatencyMs: 10,
				})
			}
		}
	}

	json.NewEncoder(w).Encode(TopologyResponse{Nodes: nodes, Edges: edges})
}

func getUserRole(r *http.Request) string {
	jwtSec := os.Getenv("SERV_JWT_SECRET")
	if jwtSec == "" && oidcIssuer == "" {
		return "admin"
	}
	role := r.Header.Get("X-Console-Role")
	if role == "" {
		return "viewer"
	}
	return role
}

func handleAuthMe(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	username := r.Header.Get("X-Console-User")
	role := getUserRole(r)
	if username == "" {
		username = "anonymous"
	}
	json.NewEncoder(w).Encode(map[string]string{
		"username": username,
		"role":     role,
	})
}

func checkProxyRBAC(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		role := getUserRole(r)
		path := r.URL.Path
		method := r.Method

		// 1. Policy edits, WASM uploads, and attach transforms (Admin only)
		isAdminRoute := (strings.HasPrefix(path, "/api/proxy/gate/api/admin/middleware/") && method != "GET") ||
			(strings.HasPrefix(path, "/api/proxy/store/console/users/") && strings.HasSuffix(path, "/policy") && method != "GET") ||
			(strings.HasPrefix(path, "/api/proxy/queue/api/topics/") && strings.HasSuffix(path, "/transform") && method != "GET")

		if isAdminRoute {
			if role != "admin" {
				WriteJSONError(w, r, "Forbidden: Admin role required for this administrative operation", "ERR_FORBIDDEN", http.StatusForbidden)
				return
			}
		}

		// 2. Queue publishes, DLQ config, and bucket write/delete (Admin or Operator only)
		isOperatorRoute := (strings.HasPrefix(path, "/api/proxy/queue/api/publish") && method == "POST") ||
			(strings.HasPrefix(path, "/api/proxy/queue/api/topics/") && strings.HasSuffix(path, "/dlq") && method == "POST") ||
			(strings.HasPrefix(path, "/api/proxy/store/") && (method == "PUT" || method == "POST" || method == "DELETE") && !strings.Contains(path, "/policy"))

		if isOperatorRoute {
			if role != "admin" && role != "operator" {
				WriteJSONError(w, r, "Forbidden: Admin or Operator role required for this write operation", "ERR_FORBIDDEN", http.StatusForbidden)
				return
			}
		}

		next(w, r)
	}
}

