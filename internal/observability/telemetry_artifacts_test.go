package observability

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestTelemetryRuleNamesStayInSync(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		body, err := os.ReadFile(filepath.Join("..", "..", path))
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	standalone := read("examples/prometheus/alerts/puppet-forge.yml")
	helm := read("deploy/puppet-forge/templates/prometheusrule.yaml")
	for _, kind := range []string{"record", "alert"} {
		pattern := regexp.MustCompile(`(?m)^\s*- ` + kind + `: (\S+)`)
		var expected, actual []string
		for _, pair := range []struct {
			source string
			names  *[]string
		}{{standalone, &expected}, {helm, &actual}} {
			for _, match := range pattern.FindAllStringSubmatch(pair.source, -1) {
				*pair.names = append(*pair.names, match[1])
			}
		}
		slices.Sort(expected)
		slices.Sort(actual)
		if !slices.Equal(expected, actual) {
			t.Fatalf("%s names differ between standalone and Helm: %v vs %v", kind, expected, actual)
		}
		for index := 1; index < len(expected); index++ {
			if expected[index] == expected[index-1] {
				t.Fatalf("duplicate %s %s", kind, expected[index])
			}
		}
	}
}

func TestComposeForwardsPublishAllowlist(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "      PUBLISH_ALLOWED_CIDRS: ${PUBLISH_ALLOWED_CIDRS:-}") {
		t.Fatal("Compose must forward the optional publish allowlist to the application")
	}
}

func TestDashboardLifecycleQueriesAndReferences(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "examples", "grafana", "puppet-forge-dashboard.json"))
	if err != nil {
		t.Fatal(err)
	}
	var dashboard struct {
		Spec struct {
			Elements  map[string]json.RawMessage `json:"elements"`
			Layout    json.RawMessage            `json:"layout"`
			Variables []struct {
				Spec struct {
					Name       string `json:"name"`
					AllValue   string `json:"allValue"`
					Definition string `json:"definition"`
					Current    struct {
						Value string `json:"value"`
					} `json:"current"`
				} `json:"spec"`
			} `json:"variables"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &dashboard); err != nil {
		t.Fatal(err)
	}
	foundJob := false
	for _, variable := range dashboard.Spec.Variables {
		if variable.Spec.Name != "job" {
			continue
		}
		foundJob = true
		if variable.Spec.AllValue != "" || variable.Spec.Current.Value != "$__all" || variable.Spec.Definition != "label_values(puppet_forge_http_requests_total, job)" {
			t.Error("job selection must discover Forge jobs and expand All without a name-based regex")
		}
	}
	if !foundJob {
		t.Fatal("dashboard has no job variable")
	}
	ids := make(map[int]bool)
	queries := ""
	for name, panel := range dashboard.Spec.Elements {
		var element struct {
			Spec struct {
				ID   int `json:"id"`
				Data struct {
					Spec struct {
						Queries []struct {
							Spec struct {
								Query struct {
									Spec struct {
										Expr string `json:"expr"`
									} `json:"spec"`
								} `json:"query"`
							} `json:"spec"`
						} `json:"queries"`
					} `json:"spec"`
				} `json:"data"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(panel, &element); err != nil {
			t.Fatal(err)
		}
		if ids[element.Spec.ID] {
			t.Fatalf("duplicate panel ID %d", element.Spec.ID)
		}
		ids[element.Spec.ID] = true
		for _, query := range element.Spec.Data.Spec.Queries {
			expression := query.Spec.Query.Spec.Expr
			queries += expression + "\n"
			if strings.Contains(expression, "last_over_time") && (strings.Contains(expression, "puppet_forge_resources") || strings.Contains(expression, "puppet_forge_module") || strings.Contains(expression, "puppet_forge_release_consumer")) {
				t.Fatalf("panel %s resurrects historical inventory", name)
			}
			if strings.Contains(expression, "time() - max(puppet_forge_upstream_refresh_last_success_timestamp_seconds") {
				t.Errorf("panel %s treats disabled or never-run refresh as epoch-aged failure", name)
			}
		}
	}
	for _, metric := range []string{"puppet_forge_resources", "puppet_forge_resource_oldest_pending_timestamp_seconds", "puppet_forge_control_operations_total", "puppet_forge_authorization_checks_total", "puppet_forge_audit_writes_total", "puppet_forge_access_config_refresh_total", "puppet_forge_security_events_total", "puppet_forge_upstream_refresh_enabled"} {
		if !strings.Contains(queries, metric) {
			t.Errorf("dashboard misses %s", metric)
		}
	}
	var layout any
	if err := json.Unmarshal(dashboard.Spec.Layout, &layout); err != nil {
		t.Fatal(err)
	}
	var checkReferences func(any)
	checkReferences = func(value any) {
		switch node := value.(type) {
		case map[string]any:
			if node["kind"] == "ElementReference" {
				name, _ := node["name"].(string)
				if _, ok := dashboard.Spec.Elements[name]; !ok {
					t.Errorf("layout references missing panel %q", name)
				}
			}
			for _, child := range node {
				checkReferences(child)
			}
		case []any:
			for _, child := range node {
				checkReferences(child)
			}
		}
	}
	checkReferences(layout)
}
