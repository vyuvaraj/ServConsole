package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRouteSerialization(t *testing.T) {
	route := Route{
		Prefix:        "/api/test",
		Target:        "http://localhost:8080",
		RateLimitRPM:  100,
		PromptGuard:   true,
		SemanticCache: false,
		PiiRedact:     true,
	}

	bytes, err := json.Marshal(route)
	if err != nil {
		t.Fatalf("Failed to marshal Route: %v", err)
	}

	var parsed Route
	if err := json.Unmarshal(bytes, &parsed); err != nil {
		t.Fatalf("Failed to unmarshal Route: %v", err)
	}

	if parsed.Prefix != route.Prefix || parsed.Target != route.Target {
		t.Errorf("Mismatch in serialized route details")
	}

	if !parsed.PromptGuard || parsed.SemanticCache || !parsed.PiiRedact {
		t.Errorf("AI middleweres state deserialized incorrectly")
	}
}

func TestHandleDbQuerySQLite(t *testing.T) {
	// 1. Create a temp SQLite db
	dbPath := "test_query_workbench.db"
	defer os.Remove(dbPath)

	// Create table and insert a row first to have data
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("Failed to open test SQLite db: %v", err)
	}
	_, err = db.Exec("CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)")
	if err != nil {
		db.Close()
		t.Fatalf("Failed to create table: %v", err)
	}
	_, err = db.Exec("INSERT INTO users (name) VALUES ('Alice'), ('Bob')")
	if err != nil {
		db.Close()
		t.Fatalf("Failed to insert rows: %v", err)
	}
	db.Close()

	// 2. Test SELECT Query via handleDbQuery
	selectPayload := `{"driver":"sqlite","connStr":"test_query_workbench.db","query":"SELECT * FROM users ORDER BY id ASC"}`
	req := httptest.NewRequest("POST", "/api/db/query", strings.NewReader(selectPayload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handleDbQuery(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected status 200, got %d", resp.StatusCode)
	}

	var res struct {
		Success  bool            `json:"success"`
		IsSelect bool            `json:"isSelect"`
		Columns  []string        `json:"columns"`
		Rows     [][]interface{} `json:"rows"`
		Error    string          `json:"error"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if !res.Success {
		t.Fatalf("Query failed: %s", res.Error)
	}

	if !res.IsSelect {
		t.Errorf("Expected isSelect to be true")
	}

	if len(res.Columns) != 2 || res.Columns[0] != "id" || res.Columns[1] != "name" {
		t.Errorf("Unexpected columns: %v", res.Columns)
	}

	if len(res.Rows) != 2 {
		t.Errorf("Expected 2 rows, got %d", len(res.Rows))
	} else {
		// Scanned row values: id is numeric (float64 in generic JSON unmarshalling), name is string
		if res.Rows[0][1] != "Alice" || res.Rows[1][1] != "Bob" {
			t.Errorf("Row data mismatch: %v", res.Rows)
		}
	}
}

func TestHandleEvents(t *testing.T) {
	oldGate := *gateUrl
	oldStore := *storeUrl
	oldQueue := *queueUrl
	defer func() {
		*gateUrl = oldGate
		*storeUrl = oldStore
		*queueUrl = oldQueue
	}()

	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer mockSrv.Close()

	*gateUrl = mockSrv.URL
	*storeUrl = mockSrv.URL
	*queueUrl = mockSrv.URL

	ctxWithCancel, cancelCtx := context.WithCancel(context.Background())
	reqWithCancel := httptest.NewRequest("GET", "/api/events", nil).WithContext(ctxWithCancel)
	
	w2 := httptest.NewRecorder()
	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		handleEvents(w2, reqWithCancel)
	}()
	
	time.Sleep(100 * time.Millisecond)
	cancelCtx() // terminate handler
	<-done2

	resp := w2.Result()
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("expected content type text/event-stream, got %s", resp.Header.Get("Content-Type"))
	}
}

func TestHandleStatusHealthz(t *testing.T) {
	oldGate := *gateUrl
	oldStore := *storeUrl
	oldQueue := *queueUrl
	oldTunnel := *tunnelUrl
	defer func() {
		*gateUrl = oldGate
		*storeUrl = oldStore
		*queueUrl = oldQueue
		*tunnelUrl = oldTunnel
	}()

	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"healthy"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mockSrv.Close()

	*gateUrl = mockSrv.URL
	*storeUrl = mockSrv.URL
	*queueUrl = mockSrv.URL
	*tunnelUrl = mockSrv.URL

	req := httptest.NewRequest("GET", "/api/status", nil)
	w := httptest.NewRecorder()

	handleStatus(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)
	resp.Body.Close()

	components, ok := result["components"].([]any)
	if !ok {
		t.Fatal("expected components list in response")
	}

	if len(components) != 4 {
		t.Fatalf("expected 4 components, got %d", len(components))
	}

	for _, c := range components {
		compMap := c.(map[string]any)
		if !compMap["online"].(bool) {
			t.Errorf("component %s expected to be online", compMap["name"])
		}
	}
}

func TestHandleAlerts(t *testing.T) {
	alertsMu.Lock()
	alerts = make([]Alert, 0)
	addOrUpdateAlert("TestComponent", "offline", "TestComponent is offline", "critical")
	alertsMu.Unlock()

	// 1. Get alerts
	req := httptest.NewRequest("GET", "/api/alerts", nil)
	w := httptest.NewRecorder()
	handleAlerts(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var alertList []Alert
	if err := json.NewDecoder(resp.Body).Decode(&alertList); err != nil {
		t.Fatalf("failed to parse alerts: %v", err)
	}

	if len(alertList) != 1 || alertList[0].Component != "TestComponent" || alertList[0].Acknowledged {
		t.Errorf("unexpected alerts list: %v", alertList)
	}

	// 2. Ack alert
	alertID := alertList[0].ID
	ackPayload := `{"id":"` + alertID + `"}`
	ackReq := httptest.NewRequest("POST", "/api/alerts/ack", strings.NewReader(ackPayload))
	ackReq.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	handleAlertsAck(w2, ackReq)

	if w2.Result().StatusCode != http.StatusOK {
		t.Fatalf("ack failed with status: %d", w2.Result().StatusCode)
	}

	alertsMu.Lock()
	if len(alerts) != 1 || !alerts[0].Acknowledged {
		t.Errorf("expected alert to be acknowledged, got: %v", alerts)
	}
	alertsMu.Unlock()
}

func TestHandleTraceReplay(t *testing.T) {
	oldTraceUrl := *traceUrl
	oldGateUrl := *gateUrl
	defer func() {
		*traceUrl = oldTraceUrl
		*gateUrl = oldGateUrl
	}()

	// Mock trace server
	mockTraceSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/traces/test-trace-id" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{
				"span": {
					"name": "POST /api/v1/test",
					"attributes": {
						"http.request.body": "{\"hello\":\"world\"}",
						"http.request.header.content-type": "application/json"
					}
				}
			}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mockTraceSrv.Close()

	// Mock gate server
	var receivedBody string
	var receivedMethod string
	mockGateSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/test" {
			receivedMethod = r.Method
			bodyBytes, _ := io.ReadAll(r.Body)
			receivedBody = string(bodyBytes)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"success"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mockGateSrv.Close()

	*traceUrl = mockTraceSrv.URL
	*gateUrl = mockGateSrv.URL

	replayPayload := `{"traceId":"test-trace-id"}`
	req := httptest.NewRequest("POST", "/api/traces/replay", strings.NewReader(replayPayload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handleTraceReplay(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var res ReplayResponse
	json.NewDecoder(resp.Body).Decode(&res)

	if !res.Success || res.StatusCode != http.StatusOK || !strings.Contains(res.Body, "success") {
		t.Errorf("unexpected replay outcome: %+v", res)
	}

	if receivedMethod != "POST" || receivedBody != `{"hello":"world"}` {
		t.Errorf("request was not replayed correctly to ServGate: method=%s body=%s", receivedMethod, receivedBody)
	}
}

func TestHandleLogs(t *testing.T) {
	logBufferMu.Lock()
	logBuffer = make([]LogEntry, 0)
	logBufferMu.Unlock()

	// 1. Ingest Log
	logPayload := `{"service":"TestSvc","level":"error","message":"Something went wrong","traceId":"test-trace-id"}`
	ingestReq := httptest.NewRequest("POST", "/api/logs/ingest", strings.NewReader(logPayload))
	ingestReq.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleIngestLog(w, ingestReq)

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("Ingest failed with status: %d", w.Result().StatusCode)
	}

	// 2. Query Logs (without filters)
	getReq := httptest.NewRequest("GET", "/api/logs", nil)
	w2 := httptest.NewRecorder()
	handleGetLogs(w2, getReq)

	var entries []LogEntry
	json.NewDecoder(w2.Body).Decode(&entries)

	if len(entries) != 1 || entries[0].Service != "TestSvc" || entries[0].Level != "error" || entries[0].Message != "Something went wrong" {
		t.Errorf("unexpected logs returned: %+v", entries)
	}

	// 3. Query Logs with filtering
	getFilteredReq := httptest.NewRequest("GET", "/api/logs?service=TestSvc&level=info", nil)
	w3 := httptest.NewRecorder()
	handleGetLogs(w3, getFilteredReq)

	var filteredEntries []LogEntry
	json.NewDecoder(w3.Body).Decode(&filteredEntries)

	if len(filteredEntries) != 0 {
		t.Errorf("expected 0 entries with level=info, got: %d", len(filteredEntries))
	}

	// 4. Query Logs with regex filtering (DX.8)
	getRegexReq := httptest.NewRequest("GET", "/api/logs?search=Some.*wrong", nil)
	w4 := httptest.NewRecorder()
	handleGetLogs(w4, getRegexReq)

	var regexEntries []LogEntry
	json.NewDecoder(w4.Body).Decode(&regexEntries)

	if len(regexEntries) != 1 {
		t.Errorf("expected 1 entry matching regex 'Some.*wrong', got: %d", len(regexEntries))
	}
}

func TestHandleCostEstimation(t *testing.T) {
	oldGate := *gateUrl
	oldStore := *storeUrl
	oldQueue := *queueUrl
	defer func() {
		*gateUrl = oldGate
		*storeUrl = oldStore
		*queueUrl = oldQueue
	}()

	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer mockSrv.Close()

	*gateUrl = mockSrv.URL
	*storeUrl = mockSrv.URL
	*queueUrl = mockSrv.URL

	req := httptest.NewRequest("GET", "/api/cost-estimation", nil)
	w := httptest.NewRecorder()
	handleCostEstimation(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	var costData map[string]any
	json.NewDecoder(resp.Body).Decode(&costData)

	monthly := costData["monthly"].(map[string]any)
	if _, ok := monthly["total"].(float64); !ok {
		t.Errorf("expected monthly total cost float64 value")
	}

	breakdown := costData["breakdown"].([]any)
	if len(breakdown) != 4 {
		t.Errorf("expected 4 breakdown categories, got %d", len(breakdown))
	}

	recs := costData["recommendations"].([]any)
	if len(recs) == 0 {
		t.Errorf("expected recommendations to be returned")
	}
}

func TestHandleSLO(t *testing.T) {
	oldGate := *gateUrl
	oldStore := *storeUrl
	oldQueue := *queueUrl
	oldTunnel := *tunnelUrl
	defer func() {
		*gateUrl = oldGate
		*storeUrl = oldStore
		*queueUrl = oldQueue
		*tunnelUrl = oldTunnel
	}()

	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer mockSrv.Close()

	*gateUrl = mockSrv.URL
	*storeUrl = mockSrv.URL
	*queueUrl = mockSrv.URL
	*tunnelUrl = mockSrv.URL

	req := httptest.NewRequest("GET", "/api/slo", nil)
	w := httptest.NewRecorder()
	handleSLO(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	var slos []SLOIndicator
	if err := json.NewDecoder(resp.Body).Decode(&slos); err != nil {
		t.Fatalf("failed to decode SLO list: %v", err)
	}

	if len(slos) != 4 {
		t.Errorf("expected 4 SLO items, got %d", len(slos))
	}

	for _, slo := range slos {
		if slo.ServiceID == "" || slo.Name == "" || slo.TargetPercent <= 0 {
			t.Errorf("invalid SLO item parsed: %+v", slo)
		}
	}
}

func TestHandleDeployments(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/deployments", nil)
	w := httptest.NewRecorder()
	handleDeployments(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var deps []Deployment
	json.NewDecoder(resp.Body).Decode(&deps)
	if len(deps) < 1 {
		t.Fatal("expected at least 1 deployment record")
	}

	// Rollback test
	rollbackPayload := `{"targetId":"dep-2"}`
	rReq := httptest.NewRequest("POST", "/api/deployments/rollback", strings.NewReader(rollbackPayload))
	rReq.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	handleRollback(w2, rReq)

	if w2.Result().StatusCode != http.StatusOK {
		t.Fatalf("rollback failed with status %d", w2.Result().StatusCode)
	}
}

func TestHandleEnvironments(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/environments", nil)
	w := httptest.NewRecorder()
	handleEnvironments(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var envData map[string]any
	json.NewDecoder(resp.Body).Decode(&envData)
	if envData["active"].(string) != "development" {
		t.Errorf("expected active env 'development', got %s", envData["active"])
	}

	// Switch environment test
	selectPayload := `{"environmentId":"staging"}`
	sReq := httptest.NewRequest("POST", "/api/environments/select", strings.NewReader(selectPayload))
	sReq.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	handleSelectEnvironment(w2, sReq)

	if w2.Result().StatusCode != http.StatusOK {
		t.Fatalf("select environment failed with status %d", w2.Result().StatusCode)
	}

	envMu.Lock()
	currEnv := activeEnvironment
	envMu.Unlock()
	if currEnv != "staging" {
		t.Errorf("expected environment to be 'staging', got %s", currEnv)
	}
}

func TestHandleIncidentAnalysis(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/incidents/analyze?alertId=alert-mock", nil)
	w := httptest.NewRecorder()
	handleIncidentAnalyze(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var timeline IncidentTimeline
	if err := json.NewDecoder(resp.Body).Decode(&timeline); err != nil {
		t.Fatalf("failed to decode incident timeline: %v", err)
	}

	if len(timeline.Events) != 4 {
		t.Errorf("expected 4 timeline events, got %d", len(timeline.Events))
	}

	foundDeploy := false
	foundAlert := false
	for _, event := range timeline.Events {
		if event.Type == "deploy" {
			foundDeploy = true
		}
		if event.Type == "alert" {
			foundAlert = true
		}
	}

	if !foundDeploy || !foundAlert {
		t.Errorf("missing critical correlated events in incident timeline: %+v", timeline.Events)
	}
}

func TestHandleRunbookExecution(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/runbooks?component=ServGate", nil)
	w := httptest.NewRecorder()
	handleRunbooks(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var actions []RunbookAction
	json.NewDecoder(resp.Body).Decode(&actions)
	if len(actions) < 1 {
		t.Fatal("expected at least one runbook action for ServGate")
	}

	execPayload := `{"runbookId":"rb-gate-cache","alertId":"alert-mock-123"}`
	eReq := httptest.NewRequest("POST", "/api/runbooks/execute", strings.NewReader(execPayload))
	eReq.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	handleExecuteRunbook(w2, eReq)

	if w2.Result().StatusCode != http.StatusOK {
		t.Fatalf("execution failed with status %d", w2.Result().StatusCode)
	}

	var execResult map[string]any
	json.NewDecoder(w2.Result().Body).Decode(&execResult)
	if !execResult["success"].(bool) {
		t.Errorf("expected runbook execution success to be true")
	}
}

func TestHandleAIMetrics(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/ai/metrics", nil)
	w := httptest.NewRecorder()
	handleAIMetrics(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var metrics AIMetricsResponse
	if err := json.NewDecoder(resp.Body).Decode(&metrics); err != nil {
		t.Fatalf("failed to decode AI metrics response: %v", err)
	}

	if metrics.TotalToolCalls <= 0 {
		t.Errorf("expected positive tool calls, got %d", metrics.TotalToolCalls)
	}
	if len(metrics.ToolCalls) == 0 {
		t.Errorf("expected tool calls array to be populated")
	}
	if len(metrics.SafetyAlerts) == 0 {
		t.Errorf("expected safety alerts array to be populated")
	}
}

func TestHandleProvisioning(t *testing.T) {
	// 1. Storage provisioning
	storePayload := `{"bucketName":"test-new-bucket"}`
	req1 := httptest.NewRequest("POST", "/api/provision/store", strings.NewReader(storePayload))
	req1.Header.Set("Content-Type", "application/json")
	w1 := httptest.NewRecorder()
	handleProvisionStore(w1, req1)

	if w1.Result().StatusCode != http.StatusOK {
		t.Fatalf("provision store failed with status %d", w1.Result().StatusCode)
	}

	var storeResult map[string]any
	json.NewDecoder(w1.Result().Body).Decode(&storeResult)
	if storeResult["bucketName"].(string) != "test-new-bucket" {
		t.Errorf("expected bucket 'test-new-bucket', got %s", storeResult["bucketName"])
	}

	// GET storage provisioning
	reqGetStore := httptest.NewRequest("GET", "/api/provision/store", nil)
	wGetStore := httptest.NewRecorder()
	handleProvisionStore(wGetStore, reqGetStore)
	var buckets []string
	json.NewDecoder(wGetStore.Result().Body).Decode(&buckets)
	foundBucket := false
	for _, b := range buckets {
		if b == "test-new-bucket" {
			foundBucket = true
			break
		}
	}
	if !foundBucket {
		t.Errorf("newly provisioned bucket not found in list")
	}

	// 2. Queue provisioning
	queuePayload := `{"topicName":"test-new-topic"}`
	req2 := httptest.NewRequest("POST", "/api/provision/queue", strings.NewReader(queuePayload))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	handleProvisionQueue(w2, req2)

	if w2.Result().StatusCode != http.StatusOK {
		t.Fatalf("provision queue failed with status %d", w2.Result().StatusCode)
	}

	var queueResult map[string]any
	json.NewDecoder(w2.Result().Body).Decode(&queueResult)
	if queueResult["topicName"].(string) != "test-new-topic" {
		t.Errorf("expected topic 'test-new-topic', got %s", queueResult["topicName"])
	}

	// GET queue provisioning
	reqGetQueue := httptest.NewRequest("GET", "/api/provision/queue", nil)
	wGetQueue := httptest.NewRecorder()
	handleProvisionQueue(wGetQueue, reqGetQueue)
	var topics []string
	json.NewDecoder(wGetQueue.Result().Body).Decode(&topics)
	foundTopic := false
	for _, t := range topics {
		if t == "test-new-topic" {
			foundTopic = true
			break
		}
	}
	if !foundTopic {
		t.Errorf("newly provisioned topic not found in list")
	}
}

func TestHandleDiagnosticExec(t *testing.T) {
	payload := `{"service":"ServGate","command":"ps aux"}`
	req := httptest.NewRequest("POST", "/api/diagnostics/exec", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleDiagnosticExec(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var res map[string]any
	json.NewDecoder(resp.Body).Decode(&res)
	if !res["success"].(bool) {
		t.Errorf("expected success to be true")
	}
	if !strings.Contains(res["output"].(string), "serv ServGate") {
		t.Errorf("expected output to contain command result, got: %s", res["output"])
	}
}

func TestHandleTopologyLive(t *testing.T) {
	// Start a mock ServStore that returns trace spans
	mockStore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/console/traces" {
			spans := []map[string]any{
				{
					"Name":         "HTTP GET /api/data",
					"TraceID":      "trace-001",
					"SpanID":       "span-001",
					"ParentSpanID": "",
					"ServiceName":  "ServGate",
					"DurationNs":   15000000,
					"StatusCode":   "ok",
					"StartTime":    time.Now().Add(-1 * time.Minute).Format(time.RFC3339Nano),
				},
				{
					"Name":         "PUT /bucket/key",
					"TraceID":      "trace-001",
					"SpanID":       "span-002",
					"ParentSpanID": "span-001",
					"ServiceName":  "ServStore",
					"DurationNs":   8000000,
					"StatusCode":   "ok",
					"StartTime":    time.Now().Add(-50 * time.Second).Format(time.RFC3339Nano),
				},
				{
					"Name":         "publish order-events",
					"TraceID":      "trace-002",
					"SpanID":       "span-003",
					"ParentSpanID": "span-001",
					"ServiceName":  "ServQueue",
					"DurationNs":   3000000,
					"StatusCode":   "error",
					"StartTime":    time.Now().Add(-40 * time.Second).Format(time.RFC3339Nano),
				},
			}
			json.NewEncoder(w).Encode(spans)
			return
		}
		w.WriteHeader(404)
	}))
	defer mockStore.Close()

	origStore := *storeUrl
	*storeUrl = mockStore.URL
	defer func() { *storeUrl = origStore }()

	req := httptest.NewRequest("GET", "/api/topology/live", nil)
	w := httptest.NewRecorder()
	handleTopologyLive(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var result LiveTopologyResponse
	json.NewDecoder(resp.Body).Decode(&result)

	if result.SpanCount != 3 {
		t.Errorf("expected 3 spans, got %d", result.SpanCount)
	}
	if result.DiscoveredAt == "" {
		t.Error("expected discovered_at to be set")
	}
	if len(result.Nodes) == 0 {
		t.Error("expected at least one node in topology")
	}

	// Verify ServGate and ServStore appear in nodes
	foundGate, foundStore := false, false
	for _, n := range result.Nodes {
		if n.ID == "ServGate" {
			foundGate = true
			if n.HealthScore < 0 || n.HealthScore > 1 {
				t.Errorf("health score out of range: %f", n.HealthScore)
			}
		}
		if n.ID == "ServStore" {
			foundStore = true
		}
	}
	if !foundGate {
		t.Error("expected ServGate node in topology")
	}
	if !foundStore {
		t.Error("expected ServStore node in topology")
	}

	// Verify at least one edge exists
	if len(result.Edges) == 0 {
		t.Error("expected at least one edge in topology")
	}
}

func TestHandleDashboards(t *testing.T) {
	// Reset dashboards to default state for testing
	dashboardsMu.Lock()
	originalDashboards := make([]Dashboard, len(dashboards))
	copy(originalDashboards, dashboards)
	dashboardsMu.Unlock()
	defer func() {
		dashboardsMu.Lock()
		dashboards = originalDashboards
		dashboardsMu.Unlock()
	}()

	// 1. GET — list existing dashboards
	req := httptest.NewRequest("GET", "/api/dashboards", nil)
	w := httptest.NewRecorder()
	handleDashboards(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET: expected status 200, got %d", resp.StatusCode)
	}

	var listed []Dashboard
	json.NewDecoder(resp.Body).Decode(&listed)
	if len(listed) == 0 {
		t.Fatal("GET: expected at least one default dashboard")
	}
	if listed[0].ID != "default-overview" {
		t.Errorf("GET: expected first dashboard ID 'default-overview', got %s", listed[0].ID)
	}

	// 2. POST — create new dashboard
	payload := `{"name":"SRE Alerts Board","description":"Custom board for SRE team","widgets":[{"id":"w1","title":"Error Spike","metric":"error_rate","chart_type":"line","time_range":"1h","service":"ServGate","width":6,"height":4}],"shared_with":["sre-team"]}`
	req = httptest.NewRequest("POST", "/api/dashboards", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	handleDashboards(w, req)

	resp = w.Result()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST: expected status 201, got %d — body: %s", resp.StatusCode, string(body))
	}

	var created Dashboard
	json.NewDecoder(resp.Body).Decode(&created)
	if created.Name != "SRE Alerts Board" {
		t.Errorf("POST: expected name 'SRE Alerts Board', got %s", created.Name)
	}
	if created.ID == "" {
		t.Error("POST: expected generated ID")
	}
	if len(created.Widgets) != 1 {
		t.Errorf("POST: expected 1 widget, got %d", len(created.Widgets))
	}

	// 3. DELETE — remove the created dashboard
	req = httptest.NewRequest("DELETE", "/api/dashboards?id="+created.ID, nil)
	w = httptest.NewRecorder()
	handleDashboards(w, req)

	resp = w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE: expected status 200, got %d", resp.StatusCode)
	}

	var delRes map[string]any
	json.NewDecoder(resp.Body).Decode(&delRes)
	if !delRes["success"].(bool) {
		t.Error("DELETE: expected success=true")
	}

	// 4. DELETE nonexistent — should 404
	req = httptest.NewRequest("DELETE", "/api/dashboards?id=nonexistent-dash", nil)
	w = httptest.NewRecorder()
	handleDashboards(w, req)

	resp = w.Result()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("DELETE nonexistent: expected 404, got %d", resp.StatusCode)
	}
}

