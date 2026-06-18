package main

import (
	"context"
	"database/sql"
	"encoding/json"
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

