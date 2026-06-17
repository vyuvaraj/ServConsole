package main

import (
	"encoding/json"
	"testing"
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
