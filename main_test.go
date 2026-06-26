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

