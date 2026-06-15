package inspect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildShowGraphReportUsesRepairedGraph(t *testing.T) {
	sessionDir := buildFixtureSession(t)
	report, err := BuildShowGraphReport(sessionDir)
	if err != nil {
		t.Fatalf("build show graph report: %v", err)
	}
	validation := report["validation"].(map[string]any)
	if validation["ok"] != true {
		t.Fatalf("validation failed: %#v", validation)
	}
	events := report["events"].([]map[string]any)
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	graph := report["graph"].(map[string]any)
	if graph["version"] != 1 {
		t.Fatalf("graph version = %v", graph["version"])
	}
	artifacts := graph["artifacts"].(map[string]any)
	if len(artifacts) != 6 {
		t.Fatalf("artifact graph entries = %d, want 6", len(artifacts))
	}
}

func TestBuildShowGraphReportSurfacesArtifactValidationErrors(t *testing.T) {
	sessionDir := buildFixtureSession(t)
	if err := os.Remove(filepath.Join(sessionDir, "artifacts", "child_results", "dynamic-child.json")); err != nil {
		t.Fatalf("remove artifact: %v", err)
	}

	report, err := BuildShowGraphReport(sessionDir)
	if err != nil {
		t.Fatalf("build show graph report: %v", err)
	}
	validation := report["validation"].(map[string]any)
	if validation["ok"] != false {
		t.Fatalf("validation unexpectedly passed: %#v", validation)
	}
	if validation["error_type"] != "ContractValidationError" {
		t.Fatalf("error_type = %v", validation["error_type"])
	}
	if !strings.Contains(validation["error"].(string), "artifact ref") {
		t.Fatalf("validation error is not explicit: %#v", validation)
	}
}

func TestBuildShowGraphReportReadsGoCreatedPhase4Fixture(t *testing.T) {
	report, err := BuildShowGraphReport(goCreatedPhase4Fixture(t))
	if err != nil {
		t.Fatalf("build show graph report: %v", err)
	}
	validation := report["validation"].(map[string]any)
	if validation["ok"] != true {
		t.Fatalf("validation failed: %#v", validation)
	}
	if validation["event_count"] != 3 {
		t.Fatalf("event_count = %v, want 3", validation["event_count"])
	}
	graph := report["graph"].(map[string]any)
	nodes := graph["nodes"].(map[string]any)
	child := nodes["relay_backend_child_go_sample"].(map[string]any)
	if child["composition_path"] != "root.slot_0" {
		t.Fatalf("composition_path = %v, want root.slot_0", child["composition_path"])
	}
	if child["status"] != "completed" {
		t.Fatalf("child status = %v, want completed", child["status"])
	}
	events := report["events"].([]map[string]any)
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3", len(events))
	}
}
