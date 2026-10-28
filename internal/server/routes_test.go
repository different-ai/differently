package server_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestOpenAPICoversRoutes keeps api/openapi.yaml in step with the router.
func TestOpenAPICoversRoutes(t *testing.T) {
	spec, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(spec, &doc); err != nil || doc["openapi"] != "3.1.0" {
		t.Fatalf("api/openapi.yaml does not parse as OpenAPI 3.1: %v", err)
	}
	var src strings.Builder
	for _, f := range []string{"server.go", "sandboxes.go", "tenancy.go", "admin.go"} {
		b, _ := os.ReadFile(f)
		src.Write(b)
	}
	routes := regexp.MustCompile(`"(GET|POST|PUT|PATCH|DELETE) (/[^"{]*(?:\{[a-z]+\}[^"{]*)*)"`).FindAllStringSubmatch(src.String(), -1)
	if len(routes) < 50 {
		t.Fatalf("found only %d routes; regexp drifted", len(routes))
	}
	for _, r := range routes {
		path := strings.TrimSuffix(r[2], "{$}")
		if path == "/" || strings.HasPrefix(path, "/dashboard") || path == "/openapi.yaml" {
			continue
		}
		if !strings.Contains(string(spec), "\n  "+path+":") {
			t.Errorf("route %s %s is missing from api/openapi.yaml", r[1], path)
		}
	}
}
