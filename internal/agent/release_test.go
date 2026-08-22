package agent

import (
	"encoding/json"
	"testing"
)

func TestOpenAPIExposesOnlyRunCommand(t *testing.T) {
	var spec struct {
		Info struct {
			Version string `json:"version"`
		} `json:"info"`
		Paths map[string]map[string]struct {
			OperationID     string `json:"operationId"`
			IsConsequential *bool  `json:"x-openai-isConsequential"`
		} `json:"paths"`
	}
	if err := json.Unmarshal([]byte(openAPISpec), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Info.Version != "0.3.0" {
		t.Fatalf("unexpected OpenAPI version: %s", spec.Info.Version)
	}
	if len(spec.Paths) != 1 {
		t.Fatalf("expected exactly one action path, got %d", len(spec.Paths))
	}
	methods, ok := spec.Paths["/v1/command/run"]
	if !ok {
		t.Fatal("runCommand path missing from OpenAPI")
	}
	op, ok := methods["post"]
	if !ok || op.OperationID != "runCommand" {
		t.Fatalf("unexpected runCommand operation: %#v", op)
	}
	if op.IsConsequential == nil || *op.IsConsequential {
		t.Fatal("runCommand must be explicitly non-consequential")
	}
}
