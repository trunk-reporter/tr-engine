package api

import (
	"strings"
	"testing"
)

func TestCollectEnvironment(t *testing.T) {
	env := collectEnvironment()

	// hostname key exists and is non-empty
	hostname, ok := env["hostname"]
	if !ok {
		t.Fatal("expected hostname key to exist")
	}
	if h, ok := hostname.(string); !ok || h == "" {
		t.Fatalf("expected hostname to be a non-empty string, got %v", hostname)
	}

	// in_container is a bool
	inContainer, ok := env["in_container"]
	if !ok {
		t.Fatal("expected in_container key to exist")
	}
	if _, ok := inContainer.(bool); !ok {
		t.Fatalf("expected in_container to be a bool, got %T", inContainer)
	}

	// go_version starts with "go"
	goVersion, ok := env["go_version"]
	if !ok {
		t.Fatal("expected go_version key to exist")
	}
	if v, ok := goVersion.(string); !ok || !strings.HasPrefix(v, "go") {
		t.Fatalf("expected go_version to start with \"go\", got %v", goVersion)
	}

	// num_cpu is > 0
	numCPU, ok := env["num_cpu"]
	if !ok {
		t.Fatal("expected num_cpu key to exist")
	}
	if n, ok := numCPU.(int); !ok || n <= 0 {
		t.Fatalf("expected num_cpu to be > 0, got %v", numCPU)
	}

	// network_interfaces key exists
	if _, ok := env["network_interfaces"]; !ok {
		t.Fatal("expected network_interfaces key to exist")
	}
}
