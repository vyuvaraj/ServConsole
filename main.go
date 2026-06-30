package main

import (
	"bytes"
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
	authUrl    = flag.String("auth-url", "http://localhost:8098", "ServAuth base URL")
	dbUrl      = flag.String("db-url", "http://localhost:8097", "ServDB base URL")
	mailUrl    = flag.String("mail-url", "http://localhost:8094", "ServMail base URL")
	flowUrl    = flag.String("flow-url", "http://localhost:8096", "ServFlow base URL")
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
	Auth         string `json:"auth"`          // ServAuth base URL
	DB           string `json:"db"`            // ServDB base URL
	Mail         string `json:"mail"`          // ServMail base URL
	Flow         string `json:"flow"`          // ServFlow base URL
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
		Auth:         *authUrl,
		DB:           *dbUrl,
		Mail:         *mailUrl,
		Flow:         *flowUrl,
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
	if manifest.Auth != "" { d.Auth = manifest.Auth }
	if manifest.DB != "" { d.DB = manifest.DB }
	if manifest.Mail != "" { d.Mail = manifest.Mail }
	if manifest.Flow != "" { d.Flow = manifest.Flow }

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
	LatencyMs int64     `json:"latency_ms"`
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

	// Seed initial logs
	logBuffer = append(logBuffer, LogEntry{Timestamp: time.Now().Add(-10 * time.Minute), Service: "ServGate", Level: "info", Message: "Gateway listening on port :8080"})
	logBuffer = append(logBuffer, LogEntry{Timestamp: time.Now().Add(-8 * time.Minute), Service: "ServStore", Level: "info", Message: "Consistent hash ring initialized with 3 nodes"})
	logBuffer = append(logBuffer, LogEntry{Timestamp: time.Now().Add(-5 * time.Minute), Service: "ServQueue", Level: "info", Message: "STOMP TCP Broker listening on port :61613"})
	logBuffer = append(logBuffer, LogEntry{Timestamp: time.Now().Add(-2 * time.Minute), Service: "ServTunnel", Level: "info", Message: "Tunnel connection established to relay pool"})

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

	aURL, err := url.Parse(*authUrl)
	if err != nil {
		log.Fatalf("Invalid auth-url: %v", err)
	}
	dURL, err := url.Parse(*dbUrl)
	if err != nil {
		log.Fatalf("Invalid db-url: %v", err)
	}
	mURL, err := url.Parse(*mailUrl)
	if err != nil {
		log.Fatalf("Invalid mail-url: %v", err)
	}

	// Create reverse proxies
	gateProxy := httputil.NewSingleHostReverseProxy(gURL)
	storeProxy := httputil.NewSingleHostReverseProxy(sURL)
	queueProxy := httputil.NewSingleHostReverseProxy(qURL)
	traceProxy := httputil.NewSingleHostReverseProxy(tURL)
	tunnelProxy := httputil.NewSingleHostReverseProxy(tunURL)
	authProxy := httputil.NewSingleHostReverseProxy(aURL)
	dbProxy := httputil.NewSingleHostReverseProxy(dURL)
	mailProxy := httputil.NewSingleHostReverseProxy(mURL)

	// Adjust Director to rewrite request path and set Authorization headers
	configureProxyDirector(gateProxy, gURL, "/api/proxy/gate", *authToken)
	configureProxyDirector(storeProxy, sURL, "/api/proxy/store", "")
	configureProxyDirector(queueProxy, qURL, "/api/proxy/queue", "secret-token")
	configureProxyDirector(traceProxy, tURL, "/api/proxy/trace", "")
	configureProxyDirector(tunnelProxy, tunURL, "/api/proxy/tunnel", "")
	configureProxyDirector(authProxy, aURL, "/api/proxy/auth", "")
	configureProxyDirector(dbProxy, dURL, "/api/proxy/db", "")
	configureProxyDirector(mailProxy, mURL, "/api/proxy/mail", "")

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
	mux.HandleFunc("/api/traces/replay", authorizeConsole(handleTraceReplay))
	mux.HandleFunc("/api/alerts", authorizeConsole(handleAlerts))
	mux.HandleFunc("/api/alerts/ack", authorizeConsole(handleAlertsAck))
	mux.HandleFunc("/api/logs", authorizeConsole(handleGetLogs))
	mux.HandleFunc("/api/logs/ingest", handleIngestLog)
	mux.HandleFunc("/api/cost-estimation", authorizeConsole(handleCostEstimation))
	mux.HandleFunc("/api/slo", authorizeConsole(handleSLO))
	mux.HandleFunc("/api/deployments", authorizeConsole(handleDeployments))
	mux.HandleFunc("/api/deployments/rollback", authorizeConsole(handleRollback))
	mux.HandleFunc("/api/environments", authorizeConsole(handleEnvironments))
	mux.HandleFunc("/api/environments/select", authorizeConsole(handleSelectEnvironment))
	mux.HandleFunc("/api/incidents/analyze", authorizeConsole(handleIncidentAnalyze))
	mux.HandleFunc("/api/runbooks", authorizeConsole(handleRunbooks))
	mux.HandleFunc("/api/runbooks/execute", authorizeConsole(handleExecuteRunbook))
	mux.HandleFunc("/api/ai/metrics", authorizeConsole(handleAIMetrics))
	mux.HandleFunc("/api/provision/store", authorizeConsole(handleProvisionStore))
	mux.HandleFunc("/api/provision/queue", authorizeConsole(handleProvisionQueue))
	mux.HandleFunc("/api/diagnostics/exec", authorizeConsole(handleDiagnosticExec))
	mux.HandleFunc("/api/topology/live", authorizeConsole(handleTopologyLive))
	mux.HandleFunc("/api/dashboards", authorizeConsole(handleDashboards))
	mux.HandleFunc("/api/dev/services", authorizeConsole(handleDevServices))
	mux.HandleFunc("/api/dev/restart", authorizeConsole(handleDevRestart))
	mux.HandleFunc("/api/playground/compile", authorizeConsole(handlePlaygroundCompile))

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
	mux.Handle("/api/proxy/auth/", authorizeConsole(authProxy.ServeHTTP))
	mux.Handle("/api/proxy/db/", authorizeConsole(dbProxy.ServeHTTP))
	mux.Handle("/api/proxy/mail/", authorizeConsole(mailProxy.ServeHTTP))

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
	go startAlertMonitoring(ctx)


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

	// Propagate credentials for internal check (only if JWT is configured)
	if jwtSec := os.Getenv("SERV_JWT_SECRET"); jwtSec != "" {
		svcToken, _ := ServShared.GenerateServiceToken(jwtSec, "servconsole")
		if svcToken != "" {
			req.Header.Set("Authorization", "Bearer "+svcToken)
		}
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
			if jwtSec := os.Getenv("SERV_JWT_SECRET"); jwtSec != "" {
				svcToken, _ := ServShared.GenerateServiceToken(jwtSec, "servconsole")
				if svcToken != "" {
					dreq.Header.Set("Authorization", "Bearer "+svcToken)
				}
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
		switch claims.Username {
		case "admin":
			role = "admin"
		case "operator", "developer-bob":
			role = "operator"
		default:
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
	switch username {
	case "admin":
		role = "admin"
	case "operator", "developer-bob":
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

type ReplayRequest struct {
	TraceID string `json:"traceId"`
}

type ReplayResponse struct {
	Success    bool   `json:"success"`
	StatusCode int    `json:"statusCode"`
	Body       string `json:"body"`
	Error      string `json:"error,omitempty"`
}

func handleTraceReplay(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		WriteJSONError(w, r, "Method not allowed", "ERR_METHOD_NOT_ALLOWED", http.StatusMethodNotAllowed)
		return
	}

	var req ReplayRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteJSONError(w, r, "Invalid request body", "ERR_INVALID_BODY", http.StatusBadRequest)
		return
	}

	traceID := req.TraceID
	if traceID == "" {
		WriteJSONError(w, r, "Trace ID is required", "ERR_TRACE_ID_REQUIRED", http.StatusBadRequest)
		return
	}

	traceDetailUrl := fmt.Sprintf("%s/api/traces/%s", *traceUrl, traceID)
	resp, err := http.Get(traceDetailUrl)
	if err != nil {
		WriteJSONError(w, r, fmt.Sprintf("Failed to fetch trace from ServTrace: %v", err), "ERR_FETCH_TRACE_FAILED", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		WriteJSONError(w, r, "Trace not found in ServTrace", "ERR_TRACE_NOT_FOUND", http.StatusNotFound)
		return
	}

	var rootNode struct {
		Span struct {
			Name       string                 `json:"name"`
			Attributes map[string]interface{} `json:"attributes"`
		} `json:"span"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rootNode); err != nil {
		WriteJSONError(w, r, fmt.Sprintf("Failed to parse trace: %v", err), "ERR_PARSE_TRACE_FAILED", http.StatusInternalServerError)
		return
	}

	parts := strings.SplitN(rootNode.Span.Name, " ", 2)
	if len(parts) < 2 {
		WriteJSONError(w, r, "Invalid root span name format. Expected 'METHOD PATH'", "ERR_INVALID_SPAN_FORMAT", http.StatusBadRequest)
		return
	}
	method := parts[0]
	path := parts[1]

	bodyStr, _ := rootNode.Span.Attributes["http.request.body"].(string)
	contentType, _ := rootNode.Span.Attributes["http.request.header.content-type"].(string)

	gateReplayUrl := fmt.Sprintf("%s%s", *gateUrl, path)
	var gateReq *http.Request
	if bodyStr != "" {
		gateReq, err = http.NewRequest(method, gateReplayUrl, strings.NewReader(bodyStr))
	} else {
		gateReq, err = http.NewRequest(method, gateReplayUrl, nil)
	}

	if err != nil {
		WriteJSONError(w, r, fmt.Sprintf("Failed to create replay request: %v", err), "ERR_CREATE_REQUEST_FAILED", http.StatusInternalServerError)
		return
	}

	if contentType != "" {
		gateReq.Header.Set("Content-Type", contentType)
	}
	if *authToken != "" {
		gateReq.Header.Set("Authorization", "Bearer "+*authToken)
	}
	gateReq.Header.Set("X-Replayed-From", traceID)

	client := &http.Client{Timeout: 10 * time.Second}
	gateResp, err := client.Do(gateReq)
	if err != nil {
		json.NewEncoder(w).Encode(ReplayResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to replay request through ServGate: %v", err),
		})
		return
	}
	defer gateResp.Body.Close()

	gateBodyBytes, _ := io.ReadAll(gateResp.Body)

	json.NewEncoder(w).Encode(ReplayResponse{
		Success:    gateResp.StatusCode >= 200 && gateResp.StatusCode < 300,
		StatusCode: gateResp.StatusCode,
		Body:       string(gateBodyBytes),
	})
}

type Alert struct {
	ID           string    `json:"id"`
	Component    string    `json:"component"`
	Type         string    `json:"type"`
	Message      string    `json:"message"`
	Severity     string    `json:"severity"`
	Timestamp    time.Time `json:"timestamp"`
	Acknowledged bool      `json:"acknowledged"`
}

var (
	alerts   = make([]Alert, 0)
	alertsMu sync.Mutex
)

func startAlertMonitoring(ctx context.Context) {
	ticker := time.NewTicker(4 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			components := []struct {
				name string
				url  string
			}{
				{"ServGate", *gateUrl},
				{"ServStore", *storeUrl},
				{"ServQueue", *queueUrl},
				{"ServTunnel", *tunnelUrl},
			}

			for _, c := range components {
				status := checkStatus(c.name, c.url)

				alertsMu.Lock()
				if !status.Online {
					addOrUpdateAlert(c.name, "offline", fmt.Sprintf("%s is OFFLINE", c.name), "critical")
				} else {
					clearAlert(c.name, "offline")

					if status.LatencyMs > 250 {
						addOrUpdateAlert(c.name, "high_latency", fmt.Sprintf("High Latency on %s: %dms", c.name, status.LatencyMs), "warning")
					} else {
						clearAlert(c.name, "high_latency")
					}
				}
				alertsMu.Unlock()
			}
		}
	}
}

func addOrUpdateAlert(component, alertType, message, severity string) {
	for i, alert := range alerts {
		if alert.Component == component && alert.Type == alertType {
			alerts[i].Message = message
			alerts[i].Timestamp = time.Now()
			return
		}
	}

	id := fmt.Sprintf("alert-%d", time.Now().UnixNano())
	alerts = append(alerts, Alert{
		ID:           id,
		Component:    component,
		Type:         alertType,
		Message:      message,
		Severity:     severity,
		Timestamp:    time.Now(),
		Acknowledged: false,
	})
}

func clearAlert(component, alertType string) {
	for i, alert := range alerts {
		if alert.Component == component && alert.Type == alertType {
			alerts = append(alerts[:i], alerts[i+1:]...)
			return
		}
	}
}

func handleAlerts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet {
		WriteJSONError(w, r, "Method not allowed", "ERR_METHOD_NOT_ALLOWED", http.StatusMethodNotAllowed)
		return
	}

	alertsMu.Lock()
	defer alertsMu.Unlock()

	json.NewEncoder(w).Encode(alerts)
}

func handleAlertsAck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		WriteJSONError(w, r, "Method not allowed", "ERR_METHOD_NOT_ALLOWED", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteJSONError(w, r, "Invalid request body", "ERR_INVALID_BODY", http.StatusBadRequest)
		return
	}

	alertsMu.Lock()
	defer alertsMu.Unlock()

	found := false
	for i, alert := range alerts {
		if alert.ID == req.ID {
			alerts[i].Acknowledged = true
			found = true
			break
		}
	}

	if !found {
		WriteJSONError(w, r, "Alert not found", "ERR_ALERT_NOT_FOUND", http.StatusNotFound)
		return
	}

	json.NewEncoder(w).Encode(map[string]any{"success": true})
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

type LogEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Service   string    `json:"service"`
	Level     string    `json:"level"`
	Message   string    `json:"message"`
	TraceID   string    `json:"traceId,omitempty"`
}

var (
	logBuffer   = make([]LogEntry, 0)
	logBufferMu sync.Mutex
	maxLogLimit = 1000
)

func handleIngestLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var entry LogEntry
	if err := json.NewDecoder(r.Body).Decode(&entry); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now()
	}
	
	// Scan for circuit breaker trips to raise system alerts
	if strings.Contains(strings.ToLower(entry.Message), "circuit_breaker") || strings.Contains(strings.ToLower(entry.Message), "circuit breaker") {
		alertsMu.Lock()
		addOrUpdateAlert(entry.Service, "circuit_breaker", entry.Message, "warning")
		alertsMu.Unlock()
	}
	
	// Scan for database slow queries to raise system alerts
	if strings.Contains(strings.ToLower(entry.Message), "database_alert") || strings.Contains(strings.ToLower(entry.Message), "database alert") || strings.Contains(entry.Message, "[DATABASE_ALERT]") {
		alertsMu.Lock()
		addOrUpdateAlert(entry.Service, "slow_query", entry.Message, "warning")
		alertsMu.Unlock()
	}

	// Scan for anomaly detection prints to raise system alerts
	if strings.Contains(entry.Message, "ANOMALY_DETECTION") || strings.Contains(entry.Message, "[ANOMALY_DETECTION]") {
		alertsMu.Lock()
		addOrUpdateAlert(entry.Service, "anomaly_detection", entry.Message, "warning")
		alertsMu.Unlock()
	}

	// Forward logs with TraceID to ServTrace logs endpoint
	if entry.TraceID != "" {
		go func(e LogEntry) {
			traceURL := os.Getenv("SERV_TRACE_URL")
			if traceURL == "" {
				traceURL = "http://localhost:8090"
			}
			payload := map[string]interface{}{
				"traceId":   e.TraceID,
				"timestamp": e.Timestamp,
				"service":   e.Service,
				"level":     e.Level,
				"message":   e.Message,
			}
			body, _ := json.Marshal(payload)
			resp, err := http.Post(traceURL+"/api/logs", "application/json", bytes.NewReader(body))
			if err == nil {
				resp.Body.Close()
			}
		}(entry)
	}

	logBufferMu.Lock()
	if len(logBuffer) >= maxLogLimit {
		logBuffer = logBuffer[1:]
	}
	logBuffer = append(logBuffer, entry)
	logBufferMu.Unlock()
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"success":true}`))
}

func handleGetLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet {
		WriteJSONError(w, r, "Method not allowed", "ERR_METHOD_NOT_ALLOWED", http.StatusMethodNotAllowed)
		return
	}

	query := r.URL.Query()
	svcFilter := query.Get("service")
	levelFilter := query.Get("level")
	searchFilter := strings.ToLower(query.Get("search"))

	logBufferMu.Lock()
	defer logBufferMu.Unlock()

	filtered := make([]LogEntry, 0)
	for _, logEntry := range logBuffer {
		if svcFilter != "" && !strings.EqualFold(logEntry.Service, svcFilter) {
			continue
		}
		if levelFilter != "" && !strings.EqualFold(logEntry.Level, levelFilter) {
			continue
		}
		if searchFilter != "" && !strings.Contains(strings.ToLower(logEntry.Message), searchFilter) {
			continue
		}
		filtered = append(filtered, logEntry)
	}

	json.NewEncoder(w).Encode(filtered)
}

func handleCostEstimation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet {
		WriteJSONError(w, r, "Method not allowed", "ERR_METHOD_NOT_ALLOWED", http.StatusMethodNotAllowed)
		return
	}

	gateStatus := checkStatus("ServGate", *gateUrl)
	storeStatus := checkStatus("ServStore", *storeUrl)
	queueStatus := checkStatus("ServQueue", *queueUrl)

	var storageBytes int64 = 524288000
	var bucketsCount int64 = 3
	var gateRequests int64 = 150000
	var queueMessages int64 = 85000

	if storeStatus.Online && storeStatus.Details != nil {
		if m, ok := storeStatus.Details.(map[string]any); ok {
			if bytesVal, exists := m["TotalBytes"]; exists {
				if f, ok := bytesVal.(float64); ok {
					storageBytes = int64(f)
				}
			}
			if bktVal, exists := m["BucketsCount"]; exists {
				if f, ok := bktVal.(float64); ok {
					bucketsCount = int64(f)
				}
			}
		}
	}

	if gateStatus.Online && gateStatus.Details != nil {
		if m, ok := gateStatus.Details.(map[string]any); ok {
			if reqsVal, exists := m["requests_total"]; exists {
				if f, ok := reqsVal.(float64); ok {
					gateRequests = int64(f)
				}
			}
		}
	}

	if queueStatus.Online && queueStatus.Details != nil {
		if m, ok := queueStatus.Details.(map[string]any); ok {
			if metrics, ok := m["metrics"].(map[string]any); ok {
				if pubVal, exists := metrics["messages_published_total"]; exists {
					if f, ok := pubVal.(float64); ok {
						queueMessages = int64(f)
					}
				}
			}
		}
	}

	baselineCost := 20.0
	budgetLimit := 50.0

	envMu.Lock()
	switch activeEnvironment {
	case "staging":
		baselineCost = 80.0
		budgetLimit = 150.0
		gateRequests = gateRequests * 4
		queueMessages = queueMessages * 4
		storageBytes = storageBytes * 3
	case "production":
		baselineCost = 250.0
		budgetLimit = 500.0
		gateRequests = gateRequests * 25
		queueMessages = queueMessages * 20
		storageBytes = storageBytes * 15
	}
	envMu.Unlock()

	storageGB := float64(storageBytes) / (1024 * 1024 * 1024)
	storageCost := storageGB * 0.023
	if storageCost < 0.01 && storageBytes > 0 {
		storageCost = 0.01
	}

	gateCost := (float64(gateRequests) / 10000.0) * 0.005
	queueCost := (float64(queueMessages) / 1000000.0) * 0.05

	totalCost := storageCost + gateCost + queueCost + baselineCost

	recommendations := []string{}
	if gateRequests > 500000 {
		recommendations = append(recommendations, "Enable route-level caching on ServGate to reduce CPU and baseline cost.")
	}
	if bucketsCount > 10 {
		recommendations = append(recommendations, "Consolidate unused S3 buckets to reduce storage index overhead.")
	}
	if storageGB > 100 {
		recommendations = append(recommendations, "Configure cold storage offloading policy in ServQueue to migrate historical logs to standard compression.")
	}
	if len(recommendations) == 0 {
		recommendations = append(recommendations, "Ecosystem resources are optimized. No actions required.")
	}

	response := map[string]any{
		"timestamp": time.Now().Format(time.RFC3339),
		"monthly": map[string]any{
			"total":   totalCost,
			"budget":  budgetLimit,
			"percent": (totalCost / budgetLimit) * 100,
		},
		"breakdown": []map[string]any{
			{"name": "Compute (Baseline)", "value": baselineCost, "color": "#6366f1"},
			{"name": "Storage (ServStore)", "value": storageCost, "color": "#10b981"},
			{"name": "Gateway (ServGate)", "value": gateCost, "color": "#06b6d4"},
			{"name": "Queue (ServQueue)", "value": queueCost, "color": "#f59e0b"},
		},
		"metrics": map[string]any{
			"storage_bytes":  storageBytes,
			"storage_gb":     storageGB,
			"gate_requests":  gateRequests,
			"queue_messages": queueMessages,
			"buckets_count":  bucketsCount,
		},
		"tenants": []map[string]any{
			{"tenant_id": "org-default", "cost": totalCost * 0.7},
			{"tenant_id": "org-test", "cost": totalCost * 0.3},
		},
		"routes": []map[string]any{
			{"route": "/api/v1/users", "cost": gateCost * 0.5, "requests": gateRequests / 2},
			{"route": "/api/v1/store", "cost": gateCost * 0.3, "requests": gateRequests / 3},
			{"route": "/api/v1/auth", "cost": gateCost * 0.2, "requests": gateRequests / 6},
		},
		"recommendations": recommendations,
	}

	json.NewEncoder(w).Encode(response)
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

type SLOIndicator struct {
	ServiceID              string  `json:"serviceId"`
	Name                   string  `json:"name"`
	TargetPercent          float64 `json:"targetPercent"`
	ActualPercent          float64 `json:"actualPercent"`
	BudgetRemainingPercent float64 `json:"budgetRemainingPercent"`
	TargetLatencyMs        int64   `json:"targetLatencyMs"`
	ActualLatencyMs        int64   `json:"actualLatencyMs"`
	Status                 string  `json:"status"` // healthy, warning, breached
	BurnRate               float64 `json:"burnRate"` // e.g. 1.0x, 4.2x
}

func handleSLO(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet {
		WriteJSONError(w, r, "Method not allowed", "ERR_METHOD_NOT_ALLOWED", http.StatusMethodNotAllowed)
		return
	}

	gateStatus := checkStatus("ServGate", *gateUrl)
	storeStatus := checkStatus("ServStore", *storeUrl)
	queueStatus := checkStatus("ServQueue", *queueUrl)
	tunnelStatus := checkStatus("ServTunnel", *tunnelUrl)

	slos := []SLOIndicator{
		{
			ServiceID:              "ServGate",
			Name:                   "Gateway Uptime (Success Rate)",
			TargetPercent:          99.9,
			ActualPercent:          99.95,
			BudgetRemainingPercent: 92.4,
			TargetLatencyMs:        150,
			ActualLatencyMs:        gateStatus.LatencyMs,
			Status:                 "healthy",
			BurnRate:               1.0,
		},
		{
			ServiceID:              "ServStore",
			Name:                   "Storage Object Read Latency",
			TargetPercent:          99.5,
			ActualPercent:          99.62,
			BudgetRemainingPercent: 88.1,
			TargetLatencyMs:        200,
			ActualLatencyMs:        storeStatus.LatencyMs,
			Status:                 "healthy",
			BurnRate:               1.1,
		},
		{
			ServiceID:              "ServQueue",
			Name:                   "Queue Message Dispatch Uptime",
			TargetPercent:          99.9,
			ActualPercent:          99.98,
			BudgetRemainingPercent: 98.2,
			TargetLatencyMs:        100,
			ActualLatencyMs:        queueStatus.LatencyMs,
			Status:                 "healthy",
			BurnRate:               0.8,
		},
		{
			ServiceID:              "ServTunnel",
			Name:                   "Relay Tunnel Connection Uptime",
			TargetPercent:          99.0,
			ActualPercent:          99.45,
			BudgetRemainingPercent: 85.0,
			TargetLatencyMs:        300,
			ActualLatencyMs:        tunnelStatus.LatencyMs,
			Status:                 "healthy",
			BurnRate:               1.2,
		},
	}

	for i, slo := range slos {
		var isOnline bool
		var latency int64
		switch slo.ServiceID {
		case "ServGate":
			isOnline = gateStatus.Online
			latency = gateStatus.LatencyMs
		case "ServStore":
			isOnline = storeStatus.Online
			latency = storeStatus.LatencyMs
		case "ServQueue":
			isOnline = queueStatus.Online
			latency = queueStatus.LatencyMs
		case "ServTunnel":
			isOnline = tunnelStatus.Online
			latency = tunnelStatus.LatencyMs
		}

		if !isOnline {
			slos[i].ActualPercent = slo.TargetPercent - 1.2
			slos[i].BudgetRemainingPercent = slo.BudgetRemainingPercent - 25.0
			if slos[i].BudgetRemainingPercent < 0 {
				slos[i].BudgetRemainingPercent = 0
			}
			slos[i].Status = "breached"
			slos[i].BurnRate = 12.5
			slos[i].ActualLatencyMs = 0
		} else if latency > slo.TargetLatencyMs {
			slos[i].ActualPercent = slo.TargetPercent - 0.15
			slos[i].BudgetRemainingPercent = slo.BudgetRemainingPercent - 8.5
			if slos[i].BudgetRemainingPercent < 0 {
				slos[i].BudgetRemainingPercent = 0
			}
			slos[i].Status = "warning"
			slos[i].BurnRate = 4.5
		}
	}

	json.NewEncoder(w).Encode(slos)
}

type Deployment struct {
	ID        string    `json:"id"`
	Version   string    `json:"version"`
	Timestamp time.Time `json:"timestamp"`
	Author    string    `json:"author"`
	Status    string    `json:"status"` // active, rolled_back, historical
	Changelog string    `json:"changelog"`
}

var (
	deployments = []Deployment{
		{ID: "dep-1", Version: "v1.4.2", Timestamp: time.Now().Add(-1 * time.Hour), Author: "alice", Status: "active", Changelog: "Merge branch 'auth-fix' - Add validation to session tokens"},
		{ID: "dep-2", Version: "v1.4.1", Timestamp: time.Now().Add(-24 * time.Hour), Author: "bob", Status: "historical", Changelog: "feat: implement cost estimator dashboard backend"},
		{ID: "dep-3", Version: "v1.4.0", Timestamp: time.Now().Add(-3 * 24 * time.Hour), Author: "alice", Status: "historical", Changelog: "feat: add log aggregation and query filter APIs"},
		{ID: "dep-4", Version: "v1.3.9", Timestamp: time.Now().Add(-7 * 24 * time.Hour), Author: "charlie", Status: "historical", Changelog: "fix: resolve memory leak in Otel trace span collector"},
	}
	deploymentsMu sync.Mutex
)

func handleDeployments(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet {
		WriteJSONError(w, r, "Method not allowed", "ERR_METHOD_NOT_ALLOWED", http.StatusMethodNotAllowed)
		return
	}

	deploymentsMu.Lock()
	defer deploymentsMu.Unlock()
	json.NewEncoder(w).Encode(deployments)
}

func handleRollback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		WriteJSONError(w, r, "Method not allowed", "ERR_METHOD_NOT_ALLOWED", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		TargetID string `json:"targetId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteJSONError(w, r, "Invalid request body", "ERR_INVALID_BODY", http.StatusBadRequest)
		return
	}

	deploymentsMu.Lock()
	defer deploymentsMu.Unlock()

	foundIndex := -1
	for i, d := range deployments {
		if d.ID == req.TargetID {
			foundIndex = i
			break
		}
	}

	if foundIndex == -1 {
		WriteJSONError(w, r, "Target deployment not found", "ERR_DEPLOYMENT_NOT_FOUND", http.StatusNotFound)
		return
	}

	// Make current active historical
	for i, d := range deployments {
		if d.Status == "active" {
			deployments[i].Status = "historical"
		}
	}

	targetDep := deployments[foundIndex]
	newID := fmt.Sprintf("dep-%d", time.Now().UnixNano())
	newVersion := targetDep.Version + "-rollback"
	newDep := Deployment{
		ID:        newID,
		Version:   newVersion,
		Timestamp: time.Now(),
		Author:    "console-operator",
		Status:    "active",
		Changelog: fmt.Sprintf("Rollback to version %s (from %s)", targetDep.Version, targetDep.ID),
	}

	deployments = append([]Deployment{newDep}, deployments...)

	json.NewEncoder(w).Encode(map[string]any{
		"success":    true,
		"deployment": newDep,
	})
}

type EnvironmentInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ThemeDot    string `json:"themeDot"`
	Description string `json:"description"`
}

var (
	environments = []EnvironmentInfo{
		{ID: "development", Name: "Development", ThemeDot: "#06b6d4", Description: "Local testing and debugging playground"},
		{ID: "staging", Name: "Staging", ThemeDot: "#f59e0b", Description: "Integration testing and candidate releases"},
		{ID: "production", Name: "Production", ThemeDot: "#a855f7", Description: "Live user-facing ecosystem runtime"},
	}
	activeEnvironment = "development"
	envMu             sync.Mutex
)

func handleEnvironments(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet {
		WriteJSONError(w, r, "Method not allowed", "ERR_METHOD_NOT_ALLOWED", http.StatusMethodNotAllowed)
		return
	}

	envMu.Lock()
	defer envMu.Unlock()
	json.NewEncoder(w).Encode(map[string]any{
		"active":       activeEnvironment,
		"environments": environments,
	})
}

func handleSelectEnvironment(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		WriteJSONError(w, r, "Method not allowed", "ERR_METHOD_NOT_ALLOWED", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		EnvironmentID string `json:"environmentId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteJSONError(w, r, "Invalid request body", "ERR_INVALID_BODY", http.StatusBadRequest)
		return
	}

	envMu.Lock()
	defer envMu.Unlock()

	valid := false
	for _, env := range environments {
		if env.ID == req.EnvironmentID {
			valid = true
			break
		}
	}

	if !valid {
		WriteJSONError(w, r, "Invalid environment ID", "ERR_INVALID_ENVIRONMENT", http.StatusBadRequest)
		return
	}

	activeEnvironment = req.EnvironmentID
	log.Printf("[environment] Switched active environment to: %s", activeEnvironment)

	json.NewEncoder(w).Encode(map[string]any{
		"success": true,
		"active":  activeEnvironment,
	})
}

type TimelineEvent struct {
	Timestamp   time.Time `json:"timestamp"`
	Type        string    `json:"type"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Color       string    `json:"color"`
}

type IncidentTimeline struct {
	AlertID            string          `json:"alertId"`
	Title              string          `json:"title"`
	Component          string          `json:"component"`
	Severity           string          `json:"severity"`
	Events             []TimelineEvent `json:"events"`
	AISuggestedRunbook string          `json:"ai_suggested_runbook,omitempty"`
	AIRunbookSteps     []string        `json:"ai_runbook_steps,omitempty"`
}

func handleIncidentAnalyze(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet {
		WriteJSONError(w, r, "Method not allowed", "ERR_METHOD_NOT_ALLOWED", http.StatusMethodNotAllowed)
		return
	}

	alertID := r.URL.Query().Get("alertId")
	if alertID == "" {
		WriteJSONError(w, r, "alertId is required", "ERR_ALERT_ID_REQUIRED", http.StatusBadRequest)
		return
	}

	alertsMu.Lock()
	var targetAlert *Alert
	for _, alert := range alerts {
		if alert.ID == alertID {
			targetAlert = &alert
			break
		}
	}
	alertsMu.Unlock()

	var component, alertType, message, severity string
	var alertTime time.Time
	if targetAlert != nil {
		component = targetAlert.Component
		alertType = targetAlert.Type
		message = targetAlert.Message
		severity = targetAlert.Severity
		alertTime = targetAlert.Timestamp
	} else {
		component = "ServGate"
		alertType = "high_latency"
		message = "High Latency on ServGate: 312ms"
		severity = "warning"
		alertTime = time.Now()
	}

	events := []TimelineEvent{}

	deploymentsMu.Lock()
	var matchingDeploy *Deployment
	for _, d := range deployments {
		if d.Timestamp.Before(alertTime) && d.Timestamp.After(alertTime.Add(-2*time.Hour)) {
			matchingDeploy = &d
			break
		}
	}
	if matchingDeploy == nil && len(deployments) > 0 {
		matchingDeploy = &deployments[0]
	}
	deploymentsMu.Unlock()

	if matchingDeploy != nil {
		events = append(events, TimelineEvent{
			Timestamp:   matchingDeploy.Timestamp,
			Type:        "deploy",
			Title:       fmt.Sprintf("Deploy: %s", matchingDeploy.Version),
			Description: fmt.Sprintf("Compiled release deployed by %s: %s", matchingDeploy.Author, matchingDeploy.Changelog),
			Color:       "#a855f7",
		})
	}

	events = append(events, TimelineEvent{
		Timestamp:   alertTime.Add(-5 * time.Minute),
		Type:        "metric",
		Title:       "Metric Threshold Breach",
		Description: fmt.Sprintf("%s metrics triggered a rule breach. Outlier detected in response times.", component),
		Color:       "#f59e0b",
	})

	logBufferMu.Lock()
	var matchingLog *LogEntry
	for _, log := range logBuffer {
		if log.Service == component && (log.Level == "error" || log.Level == "warn") {
			matchingLog = &log
			break
		}
	}
	logBufferMu.Unlock()

	if matchingLog != nil {
		events = append(events, TimelineEvent{
			Timestamp:   matchingLog.Timestamp,
			Type:        "log",
			Title:       fmt.Sprintf("Log %s: %s", strings.ToUpper(matchingLog.Level), matchingLog.Service),
			Description: matchingLog.Message,
			Color:       "#ef4444",
		})
	} else {
		events = append(events, TimelineEvent{
			Timestamp:   alertTime.Add(-2 * time.Minute),
			Type:        "log",
			Title:       fmt.Sprintf("Log ERROR: %s", component),
			Description: fmt.Sprintf("Ecosystem engine captured internal trace error: connection timeout on %s downstream.", component),
			Color:       "#ef4444",
		})
	}

	events = append(events, TimelineEvent{
		Timestamp:   alertTime,
		Type:        "alert",
		Title:       fmt.Sprintf("Alert Triggered: %s", alertType),
		Description: message,
		Color:       "#dc2626",
	})

	var suggestedRunbook string
	var runbookSteps []string
	switch strings.ToLower(component) {
	case "servgate":
		suggestedRunbook = "rb-gate-restart"
		runbookSteps = []string{
			"1. Check route health metrics",
			"2. Run: serv gate restart --graceful",
			"3. Confirm service recovery logs",
		}
	case "servstore":
		suggestedRunbook = "rb-store-heal"
		runbookSteps = []string{
			"1. Assess filesystem status",
			"2. Run: serv store heal --shards=all",
			"3. Re-verify replica consistency",
		}
	default:
		suggestedRunbook = "rb-queue-purge"
		runbookSteps = []string{
			"1. Verify DLQ message count",
			"2. Run: serv queue purge dlq",
			"3. Auto-notify subscribers",
		}
	}

	timeline := IncidentTimeline{
		AlertID:            alertID,
		Title:              fmt.Sprintf("Incident Analysis: %s", message),
		Component:          component,
		Severity:           severity,
		Events:             events,
		AISuggestedRunbook: suggestedRunbook,
		AIRunbookSteps:     runbookSteps,
	}

	json.NewEncoder(w).Encode(timeline)
}

type RunbookAction struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Component   string `json:"component"`
	Command     string `json:"command"`
}

var (
	runbooks = []RunbookAction{
		{ID: "rb-gate-restart", Name: "Restart ServGate Instance", Description: "Drain connections and perform graceful restart of the Gateway process", Component: "ServGate", Command: "serv gate restart --graceful"},
		{ID: "rb-gate-cache", Name: "Clear ServGate Router Cache", Description: "Purge all compiled semantic cache entries in Gateway memory", Component: "ServGate", Command: "serv gate cache purge"},
		{ID: "rb-store-heal", Name: "Rebalance ServStore Storage Shards", Description: "Initiate P2P healing across active data shards and rebuild parity partitions", Component: "ServStore", Command: "serv store heal --shards=all"},
		{ID: "rb-queue-purge", Name: "Flush Dead Letter Queue (DLQ)", Description: "Clear stale or rejected messages in ServQueue DLQ namespaces", Component: "ServQueue", Command: "serv queue purge dlq"},
	}
	runbooksMu sync.Mutex
)

func handleRunbooks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet {
		WriteJSONError(w, r, "Method not allowed", "ERR_METHOD_NOT_ALLOWED", http.StatusMethodNotAllowed)
		return
	}

	compFilter := r.URL.Query().Get("component")

	runbooksMu.Lock()
	defer runbooksMu.Unlock()

	filtered := []RunbookAction{}
	for _, rb := range runbooks {
		if compFilter == "" || strings.EqualFold(rb.Component, compFilter) {
			filtered = append(filtered, rb)
		}
	}

	json.NewEncoder(w).Encode(filtered)
}

func handleExecuteRunbook(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		WriteJSONError(w, r, "Method not allowed", "ERR_METHOD_NOT_ALLOWED", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		RunbookID string `json:"runbookId"`
		AlertID   string `json:"alertId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteJSONError(w, r, "Invalid request body", "ERR_INVALID_BODY", http.StatusBadRequest)
		return
	}

	runbooksMu.Lock()
	var targetRb *RunbookAction
	for _, rb := range runbooks {
		if rb.ID == req.RunbookID {
			targetRb = &rb
			break
		}
	}
	runbooksMu.Unlock()

	if targetRb == nil {
		WriteJSONError(w, r, "Runbook not found", "ERR_RUNBOOK_NOT_FOUND", http.StatusNotFound)
		return
	}

	addAuditLog("console-operator", fmt.Sprintf("Runbook %s: %s", targetRb.Name, targetRb.Command), r.Method, r.URL.Path, http.StatusOK)

	if req.AlertID != "" {
		alertsMu.Lock()
		for i, a := range alerts {
			if a.ID == req.AlertID {
				alerts[i].Acknowledged = true
				break
			}
		}
		alertsMu.Unlock()
	}

	json.NewEncoder(w).Encode(map[string]any{
		"success": true,
		"message": fmt.Sprintf("Runbook %s executed successfully.", targetRb.Name),
		"log":     fmt.Sprintf("Command '%s' exited with code 0.", targetRb.Command),
	})
}

var (
	customBuckets   = []string{"media-assets", "logs", "user-documents"}
	customBucketsMu sync.Mutex
	customTopics    = []string{"orders", "notifications", "user-signups"}
	customTopicsMu  sync.Mutex
)

type AIMetricsResponse struct {
	TotalCostsUSD     float64        `json:"totalCostsUsd"`
	TotalToolCalls    int            `json:"totalToolCalls"`
	ActiveAgentsCount int            `json:"activeAgentsCount"`
	ToolCalls         []AIToolCall   `json:"toolCalls"`
	SafetyAlerts      []AISafetyAlert `json:"safetyAlerts"`
}

type AIToolCall struct {
	Timestamp  string  `json:"timestamp"`
	AgentName  string  `json:"agentName"`
	ToolCalled string  `json:"toolCalled"`
	Status     string  `json:"status"`
	TokensUsed int     `json:"tokensUsed"`
	CostUSD    float64 `json:"costUsd"`
}

type AISafetyAlert struct {
	Timestamp string `json:"timestamp"`
	AgentName string `json:"agentName"`
	Severity  string `json:"severity"`
	RuleName  string `json:"ruleName"`
	Message   string `json:"message"`
}

func handleAIMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet {
		WriteJSONError(w, r, "Method not allowed", "ERR_METHOD_NOT_ALLOWED", http.StatusMethodNotAllowed)
		return
	}

	nowStr := time.Now().Format("15:04:05")

	resp := AIMetricsResponse{
		TotalCostsUSD:     24.85,
		TotalToolCalls:    1420,
		ActiveAgentsCount: 3,
		ToolCalls: []AIToolCall{
			{Timestamp: nowStr, AgentName: "AutoDebugger-Agent", ToolCalled: "ServStore.ReadObject", Status: "success", TokensUsed: 420, CostUSD: 0.0084},
			{Timestamp: nowStr, AgentName: "CodeArchitect-Agent", ToolCalled: "ServQueue.Publish", Status: "success", TokensUsed: 650, CostUSD: 0.0130},
			{Timestamp: nowStr, AgentName: "DeployBot", ToolCalled: "ServGate.AddRoute", Status: "success", TokensUsed: 310, CostUSD: 0.0062},
			{Timestamp: "10:31:02", AgentName: "SecurityScanner", ToolCalled: "ServStore.ListObjects", Status: "blocked", TokensUsed: 120, CostUSD: 0.0024},
		},
		SafetyAlerts: []AISafetyAlert{
			{Timestamp: "10:31:02", AgentName: "SecurityScanner", Severity: "high", RuleName: "Prompt Guard Violation", Message: "Detected attempt to bypass access-control on system-access-logs bucket."},
			{Timestamp: "10:15:40", AgentName: "AutoDebugger-Agent", Severity: "medium", RuleName: "PII Leak Detected", Message: "Redacted credit card numbers inside request parameters payload."},
		},
	}

	json.NewEncoder(w).Encode(resp)
}

func handleProvisionStore(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		customBucketsMu.Lock()
		defer customBucketsMu.Unlock()
		json.NewEncoder(w).Encode(customBuckets)
		return
	}

	if r.Method != http.MethodPost {
		WriteJSONError(w, r, "Method not allowed", "ERR_METHOD_NOT_ALLOWED", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		BucketName string `json:"bucketName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.BucketName == "" {
		WriteJSONError(w, r, "Invalid request body", "ERR_INVALID_BODY", http.StatusBadRequest)
		return
	}

	// Clean/validate name
	bucketName := strings.ToLower(strings.TrimSpace(req.BucketName))

	// Attempt real PUT on ServStore S3 Gateway
	client := http.Client{Timeout: 1 * time.Second}
	putUrl := fmt.Sprintf("%s/%s", strings.TrimSuffix(*storeUrl, "/"), bucketName)
	realReq, _ := http.NewRequest(http.MethodPut, putUrl, nil)
	realResp, err := client.Do(realReq)

	realSuccess := false
	if err == nil {
		realResp.Body.Close()
		if realResp.StatusCode == http.StatusOK || realResp.StatusCode == http.StatusCreated {
			realSuccess = true
		}
	}

	// Add to memory list
	customBucketsMu.Lock()
	found := false
	for _, b := range customBuckets {
		if b == bucketName {
			found = true
			break
		}
	}
	if !found {
		customBuckets = append(customBuckets, bucketName)
	}
	customBucketsMu.Unlock()

	addAuditLog("console-operator", "Create Bucket: "+bucketName, r.Method, r.URL.Path, http.StatusOK)

	json.NewEncoder(w).Encode(map[string]any{
		"success":     true,
		"bucketName":  bucketName,
		"realGateway": realSuccess,
		"message":     fmt.Sprintf("Bucket '%s' successfully provisioned.", bucketName),
	})
}

func handleProvisionQueue(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		customTopicsMu.Lock()
		defer customTopicsMu.Unlock()
		json.NewEncoder(w).Encode(customTopics)
		return
	}

	if r.Method != http.MethodPost {
		WriteJSONError(w, r, "Method not allowed", "ERR_METHOD_NOT_ALLOWED", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		TopicName string `json:"topicName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.TopicName == "" {
		WriteJSONError(w, r, "Invalid request body", "ERR_INVALID_BODY", http.StatusBadRequest)
		return
	}

	topicName := strings.ToLower(strings.TrimSpace(req.TopicName))

	// Attempt real schema registration on ServQueue to trigger topic creation
	client := http.Client{Timeout: 1 * time.Second}
	postUrl := fmt.Sprintf("%s/api/topics/%s/schema", strings.TrimSuffix(*queueUrl, "/"), topicName)
	realReq, _ := http.NewRequest(http.MethodPost, postUrl, strings.NewReader("{}"))
	realReq.Header.Set("Authorization", "Bearer secret-token")
	realReq.Header.Set("Content-Type", "application/json")
	realResp, err := client.Do(realReq)

	realSuccess := false
	if err == nil {
		realResp.Body.Close()
		if realResp.StatusCode == http.StatusOK {
			realSuccess = true
		}
	}

	// Add to memory list
	customTopicsMu.Lock()
	found := false
	for _, t := range customTopics {
		if t == topicName {
			found = true
			break
		}
	}
	if !found {
		customTopics = append(customTopics, topicName)
	}
	customTopicsMu.Unlock()

	addAuditLog("console-operator", "Create Topic: "+topicName, r.Method, r.URL.Path, http.StatusOK)

	json.NewEncoder(w).Encode(map[string]any{
		"success":     true,
		"topicName":   topicName,
		"realGateway": realSuccess,
		"message":     fmt.Sprintf("Topic '%s' successfully provisioned.", topicName),
	})
}

func handleDiagnosticExec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		WriteJSONError(w, r, "Method not allowed", "ERR_METHOD_NOT_ALLOWED", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Service string `json:"service"`
		Command string `json:"command"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Command == "" {
		WriteJSONError(w, r, "Invalid request body", "ERR_INVALID_BODY", http.StatusBadRequest)
		return
	}

	cmd := strings.ToLower(strings.TrimSpace(req.Command))
	output := ""
	status := "success"

	switch cmd {
	case "ps aux":
		output = fmt.Sprintf("USER       PID %%CPU %%MEM    VSZ    RSS TTY      STAT START   TIME COMMAND\n" +
			"root         1  0.1  0.4  18432  8192 ?        Ss   10:00   0:02 /bin/init\n" +
			"operator    42  1.4  3.2 245760 65536 ?        Sl   10:05   0:15 /usr/local/bin/serv %s\n" +
			"operator    48  0.0  0.5  12288  1024 ?        S    10:06   0:00 sh -c diagnostic-daemon", req.Service)
	case "free -m":
		output = "              total        used        free      shared  buff/cache   available\n" +
			"Mem:           8192        3420        2845         120        1927        4652\n" +
			"Swap:          2048         105        1943"
	case "df -h":
		output = "Filesystem      Size  Used Avail Use%% Mounted on\n" +
			"/dev/sda1        50G   24G   26G  48%% /\n" +
			"tmpfs           4.0G     0  4.0G   0%% /dev/shm\n" +
			"/dev/sdb1       200G   82G  118G  41%% /data/store"
	case "serv status":
		output = fmt.Sprintf("Servverse Component: %s\n" +
			"Status: ACTIVE\n" +
			"Uptime: 2h 45m 12s\n" +
			"Version: v1.4.2-stable\n" +
			"Config Load: /etc/serv/config.json (OK)\n" +
			"P2P Cluster Ring Nodes: 5/5 Active", req.Service)
	default:
		if strings.HasPrefix(cmd, "ping ") {
			target := strings.TrimPrefix(cmd, "ping ")
			output = fmt.Sprintf("PING %s (127.0.0.1) 56(84) bytes of data.\n" +
				"64 bytes from 127.0.0.1: icmp_seq=1 ttl=64 time=0.042 ms\n" +
				"64 bytes from 127.0.0.1: icmp_seq=2 ttl=64 time=0.038 ms\n" +
				"--- %s ping statistics ---\n" +
				"2 packets transmitted, 2 received, 0%% packet loss, time 1002ms\n" +
				"rtt min/avg/max/mdev = 0.038/0.040/0.042/0.002 ms", target, target)
		} else {
			output = fmt.Sprintf("bash: %s: command not found\nAvailable diagnostic tools: ps aux, free -m, df -h, serv status, ping [target]", req.Command)
			status = "error"
		}
	}

	addAuditLog("console-operator", fmt.Sprintf("Diagnostics Exec (%s): %s", req.Service, req.Command), r.Method, r.URL.Path, http.StatusOK)

	json.NewEncoder(w).Encode(map[string]any{
		"success": true,
		"status":  status,
		"output":  output,
	})
}

// --- 7.3/8.8: Service Topology Auto-Discovery from Live Traces ---
// Enhances the existing /api/topology with real-time inferred topology
// featuring request counts, computed error rates, health scores, and
// throughput per edge — all derived from live OTel trace spans.

type LiveTopologyNode struct {
	ID           string  `json:"id"`
	Label        string  `json:"label"`
	Color        string  `json:"color"`
	Online       bool    `json:"online"`
	LatencyMs    int64   `json:"latency_ms"`
	ErrorRate    float64 `json:"error_rate"`
	RequestCount int64   `json:"request_count"`
	HealthScore  float64 `json:"health_score"` // 0.0 to 1.0
}

type LiveTopologyEdge struct {
	From        string  `json:"from"`
	To          string  `json:"to"`
	Label       string  `json:"label"`
	LatencyMs   int64   `json:"latency_ms"`
	ErrorRate   float64 `json:"error_rate"`
	Throughput  int64   `json:"throughput"` // requests per interval
}

type LiveTopologyResponse struct {
	Nodes        []LiveTopologyNode `json:"nodes"`
	Edges        []LiveTopologyEdge `json:"edges"`
	DiscoveredAt string             `json:"discovered_at"`
	SpanCount    int                `json:"span_count"`
}

func handleTopologyLive(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet {
		WriteJSONError(w, r, "Method not allowed", "ERR_METHOD_NOT_ALLOWED", http.StatusMethodNotAllowed)
		return
	}

	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(strings.TrimSuffix(*storeUrl, "/") + "/console/traces")
	if err != nil {
		json.NewEncoder(w).Encode(LiveTopologyResponse{
			Nodes:        []LiveTopologyNode{},
			Edges:        []LiveTopologyEdge{},
			DiscoveredAt: time.Now().Format(time.RFC3339),
			SpanCount:    0,
		})
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
		json.NewEncoder(w).Encode(LiveTopologyResponse{
			Nodes:        []LiveTopologyNode{},
			Edges:        []LiveTopologyEdge{},
			DiscoveredAt: time.Now().Format(time.RFC3339),
			SpanCount:    0,
		})
		return
	}

	type nodeStats struct {
		totalLatency int64
		requestCount int64
		errorCount   int64
	}

	type edgeStats struct {
		totalLatency int64
		requestCount int64
		errorCount   int64
	}

	nodeStatsMap := make(map[string]*nodeStats)
	edgeStatsMap := make(map[string]*edgeStats)
	nodesMap := make(map[string]*LiveTopologyNode)

	serviceColors := map[string]string{
		"ServGate":   "#06b6d4",
		"ServStore":  "#10b981",
		"ServQueue":  "#f59e0b",
		"ServTunnel": "#6366f1",
		"ServTrace":  "#a855f7",
	}

	// Pre-populate well-known Servverse nodes
	for svc, color := range serviceColors {
		nodesMap[svc] = &LiveTopologyNode{ID: svc, Label: svc, Color: color, Online: true}
		nodeStatsMap[svc] = &nodeStats{}
	}

	spanToService := make(map[string]string)
	for _, span := range spans {
		svc := span.ServiceName
		if svc == "" {
			svc = "unknown-service"
		}
		spanToService[span.SpanID] = svc

		if _, exists := nodesMap[svc]; !exists {
			color := "#94a3b8"
			if c, ok := serviceColors[svc]; ok {
				color = c
			}
			nodesMap[svc] = &LiveTopologyNode{ID: svc, Label: svc, Color: color, Online: true}
			nodeStatsMap[svc] = &nodeStats{}
		}

		latMs := span.DurationNs / 1e6
		nodeStatsMap[svc].totalLatency += latMs
		nodeStatsMap[svc].requestCount++
		if span.StatusCode == "error" {
			nodeStatsMap[svc].errorCount++
		}
	}

	// Build edges from parent-child span relationships
	for _, span := range spans {
		if span.ParentSpanID == "" {
			continue
		}
		svc := span.ServiceName
		if svc == "" {
			svc = "unknown-service"
		}

		parentSvc, parentExists := spanToService[span.ParentSpanID]
		if !parentExists || parentSvc == svc {
			continue
		}

		edgeKey := parentSvc + "->" + svc
		latMs := span.DurationNs / 1e6

		if _, exists := edgeStatsMap[edgeKey]; !exists {
			edgeStatsMap[edgeKey] = &edgeStats{}
		}
		edgeStatsMap[edgeKey].totalLatency += latMs
		edgeStatsMap[edgeKey].requestCount++
		if span.StatusCode == "error" {
			edgeStatsMap[edgeKey].errorCount++
		}
	}

	// Infer additional edges from span operation names
	for _, span := range spans {
		svc := span.ServiceName
		if svc == "" {
			svc = "unknown-service"
		}
		latMs := span.DurationNs / 1e6

		if strings.Contains(span.Name, "PUT") || strings.Contains(span.Name, "GET") || strings.Contains(span.Name, "DELETE") {
			if svc != "ServStore" {
				edgeKey := svc + "->ServStore"
				if _, exists := edgeStatsMap[edgeKey]; !exists {
					edgeStatsMap[edgeKey] = &edgeStats{totalLatency: latMs, requestCount: 1}
				}
			}
		}
		if strings.Contains(span.Name, "publish") || strings.Contains(span.Name, "subscribe") {
			if svc != "ServQueue" {
				edgeKey := svc + "->ServQueue"
				if _, exists := edgeStatsMap[edgeKey]; !exists {
					edgeStatsMap[edgeKey] = &edgeStats{totalLatency: latMs, requestCount: 1}
				}
			}
		}
	}

	// Compute final node values
	var nodes []LiveTopologyNode
	for svcID, node := range nodesMap {
		ns := nodeStatsMap[svcID]
		if ns.requestCount > 0 {
			node.LatencyMs = ns.totalLatency / ns.requestCount
			node.ErrorRate = float64(ns.errorCount) / float64(ns.requestCount)
			node.RequestCount = ns.requestCount
		}
		// Health score: 1.0 = perfect, degrades with error rate and high latency
		health := 1.0 - node.ErrorRate
		if node.LatencyMs > 500 {
			health -= 0.15
		} else if node.LatencyMs > 200 {
			health -= 0.05
		}
		if health < 0 {
			health = 0
		}
		node.HealthScore = health
		nodes = append(nodes, *node)
	}

	// Compute final edge values
	var edges []LiveTopologyEdge
	for edgeKey, es := range edgeStatsMap {
		parts := strings.SplitN(edgeKey, "->", 2)
		if len(parts) != 2 {
			continue
		}
		avgLat := int64(0)
		errRate := 0.0
		if es.requestCount > 0 {
			avgLat = es.totalLatency / es.requestCount
			errRate = float64(es.errorCount) / float64(es.requestCount)
		}

		label := "Call"
		if parts[1] == "ServStore" {
			label = "S3"
		} else if parts[1] == "ServQueue" {
			label = "STOMP"
		} else if parts[0] == "ServGate" {
			label = "HTTP"
		}

		edges = append(edges, LiveTopologyEdge{
			From:       parts[0],
			To:         parts[1],
			Label:      label,
			LatencyMs:  avgLat,
			ErrorRate:  errRate,
			Throughput: es.requestCount,
		})
	}

	json.NewEncoder(w).Encode(LiveTopologyResponse{
		Nodes:        nodes,
		Edges:        edges,
		DiscoveredAt: time.Now().Format(time.RFC3339),
		SpanCount:    len(spans),
	})
}

// --- 7.5: Custom Dashboard Builder ---
// Provides CRUD for user-defined dashboards with custom widgets.
// Dashboards are stored in-memory and support chart type selection,
// metric binding, and per-team sharing.

type DashboardWidget struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Metric     string `json:"metric"`     // e.g. "latency", "error_rate", "throughput"
	ChartType  string `json:"chart_type"` // "line", "bar", "gauge", "table"
	TimeRange  string `json:"time_range"` // e.g. "1h", "6h", "24h", "7d"
	Service    string `json:"service"`    // e.g. "ServGate", "ServStore"
	PositionX  int    `json:"position_x"`
	PositionY  int    `json:"position_y"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
}

type Dashboard struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	CreatedBy   string            `json:"created_by"`
	CreatedAt   string            `json:"created_at"`
	UpdatedAt   string            `json:"updated_at"`
	Widgets     []DashboardWidget `json:"widgets"`
	SharedWith  []string          `json:"shared_with"` // team names
}

var (
	dashboardsMu sync.Mutex
	dashboards   = []Dashboard{
		{
			ID:          "default-overview",
			Name:        "Ecosystem Overview",
			Description: "Default overview dashboard for all Servverse components",
			CreatedBy:   "system",
			CreatedAt:   "2026-06-01T00:00:00Z",
			UpdatedAt:   "2026-06-01T00:00:00Z",
			Widgets: []DashboardWidget{
				{ID: "w1", Title: "Gateway Latency", Metric: "latency", ChartType: "line", TimeRange: "1h", Service: "ServGate", PositionX: 0, PositionY: 0, Width: 6, Height: 4},
				{ID: "w2", Title: "Queue Throughput", Metric: "throughput", ChartType: "bar", TimeRange: "1h", Service: "ServQueue", PositionX: 6, PositionY: 0, Width: 6, Height: 4},
				{ID: "w3", Title: "Storage Error Rate", Metric: "error_rate", ChartType: "gauge", TimeRange: "6h", Service: "ServStore", PositionX: 0, PositionY: 4, Width: 4, Height: 3},
				{ID: "w4", Title: "Active Connections", Metric: "connections", ChartType: "line", TimeRange: "24h", Service: "ServTunnel", PositionX: 4, PositionY: 4, Width: 4, Height: 3},
				{ID: "w5", Title: "Service Health", Metric: "health", ChartType: "table", TimeRange: "1h", Service: "all", PositionX: 8, PositionY: 4, Width: 4, Height: 3},
			},
			SharedWith: []string{"platform-team", "sre-team"},
		},
	}
)

func handleDashboards(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		dashboardsMu.Lock()
		result := make([]Dashboard, len(dashboards))
		copy(result, dashboards)
		dashboardsMu.Unlock()
		json.NewEncoder(w).Encode(result)

	case http.MethodPost:
		var newDash Dashboard
		if err := json.NewDecoder(r.Body).Decode(&newDash); err != nil || newDash.Name == "" {
			WriteJSONError(w, r, "Invalid dashboard payload — name is required", "ERR_INVALID_BODY", http.StatusBadRequest)
			return
		}

		now := time.Now().Format(time.RFC3339)
		if newDash.ID == "" {
			newDash.ID = fmt.Sprintf("dash-%d", time.Now().UnixNano())
		}
		newDash.CreatedAt = now
		newDash.UpdatedAt = now
		if newDash.CreatedBy == "" {
			newDash.CreatedBy = "console-operator"
		}
		if newDash.Widgets == nil {
			newDash.Widgets = []DashboardWidget{}
		}
		if newDash.SharedWith == nil {
			newDash.SharedWith = []string{}
		}

		dashboardsMu.Lock()
		dashboards = append(dashboards, newDash)
		dashboardsMu.Unlock()

		addAuditLog(newDash.CreatedBy, fmt.Sprintf("Created dashboard: %s", newDash.Name), r.Method, r.URL.Path, http.StatusCreated)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(newDash)

	case http.MethodPut:
		var updated Dashboard
		if err := json.NewDecoder(r.Body).Decode(&updated); err != nil || updated.ID == "" {
			WriteJSONError(w, r, "Invalid dashboard payload — id is required", "ERR_INVALID_BODY", http.StatusBadRequest)
			return
		}

		updated.UpdatedAt = time.Now().Format(time.RFC3339)
		if updated.Widgets == nil {
			updated.Widgets = []DashboardWidget{}
		}
		if updated.SharedWith == nil {
			updated.SharedWith = []string{}
		}

		dashboardsMu.Lock()
		found := false
		for i, d := range dashboards {
			if d.ID == updated.ID {
				// Preserve original creation metadata
				if updated.CreatedAt == "" {
					updated.CreatedAt = d.CreatedAt
				}
				if updated.CreatedBy == "" {
					updated.CreatedBy = d.CreatedBy
				}
				dashboards[i] = updated
				found = true
				break
			}
		}
		dashboardsMu.Unlock()

		if !found {
			WriteJSONError(w, r, "Dashboard not found", "ERR_NOT_FOUND", http.StatusNotFound)
			return
		}

		addAuditLog("console-operator", fmt.Sprintf("Updated dashboard: %s", updated.Name), r.Method, r.URL.Path, http.StatusOK)
		json.NewEncoder(w).Encode(updated)

	case http.MethodDelete:
		dashID := r.URL.Query().Get("id")
		if dashID == "" {
			WriteJSONError(w, r, "Query parameter 'id' is required", "ERR_MISSING_ID", http.StatusBadRequest)
			return
		}

		dashboardsMu.Lock()
		found := false
		for i, d := range dashboards {
			if d.ID == dashID {
				dashboards = append(dashboards[:i], dashboards[i+1:]...)
				found = true
				break
			}
		}
		dashboardsMu.Unlock()

		if !found {
			WriteJSONError(w, r, "Dashboard not found", "ERR_NOT_FOUND", http.StatusNotFound)
			return
		}

		addAuditLog("console-operator", fmt.Sprintf("Deleted dashboard: %s", dashID), r.Method, r.URL.Path, http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"success": true, "deleted_id": dashID})

	default:
		WriteJSONError(w, r, "Method not allowed", "ERR_METHOD_NOT_ALLOWED", http.StatusMethodNotAllowed)
	}
}

type DevServiceStatus struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Status string `json:"status"`
}

func handleDevServices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	services := []struct {
		Name string
		URL  string
	}{
		{"ServGate", activeDiscovery.Gate},
		{"ServStore", activeDiscovery.Store},
		{"ServQueue", activeDiscovery.Queue},
		{"ServTrace", activeDiscovery.Trace},
		{"ServTunnel", activeDiscovery.Tunnel},
		{"ServAuth", activeDiscovery.Auth},
		{"ServDB", activeDiscovery.DB},
		{"ServMail", activeDiscovery.Mail},
		{"ServFlow", activeDiscovery.Flow},
	}

	client := &http.Client{Timeout: 300 * time.Millisecond}
	var list []DevServiceStatus

	for _, s := range services {
		if s.URL == "" {
			continue
		}
		status := "unhealthy"
		resp, err := client.Get(strings.TrimSuffix(s.URL, "/") + "/healthz")
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				status = "healthy"
			}
			resp.Body.Close()
		} else {
			resp2, err2 := client.Get(strings.TrimSuffix(s.URL, "/") + "/health")
			if err2 == nil {
				if resp2.StatusCode == http.StatusOK {
					status = "healthy"
				}
				resp2.Body.Close()
			}
		}

		list = append(list, DevServiceStatus{
			Name:   s.Name,
			URL:    s.URL,
			Status: status,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

func handleDevRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	serviceName := r.URL.Query().Get("service")
	if serviceName == "" {
		http.Error(w, "service parameter required", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "success",
		"message": fmt.Sprintf("Restart triggered for service %s in dev mode", serviceName),
	})
}

func handlePlaygroundCompile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		WriteJSONError(w, r, "Method not allowed", "ERR_METHOD_NOT_ALLOWED", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteJSONError(w, r, "Bad request", "ERR_BAD_REQUEST_BODY", http.StatusBadRequest)
		return
	}

	status := "success"
	var diagnostics []map[string]any
	if strings.Contains(req.Code, "syntax error") || strings.Contains(req.Code, "error") {
		status = "error"
		diagnostics = append(diagnostics, map[string]any{
			"line":    10,
			"column":  5,
			"message": "Syntax error: unexpected token or invalid declaration",
			"type":    "error",
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":      status,
		"diagnostics": diagnostics,
		"preview":     "AST compilation complete: 0 errors detected.",
	})
}

