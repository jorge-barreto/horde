package bootstrap

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRender_EmptySlug(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"whitespace", "   "},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := Render(tc.in)
			if err == nil {
				t.Fatalf("expected error for slug %q, got nil", tc.in)
			}
			if !strings.Contains(err.Error(), "slug is empty") {
				t.Errorf("error message %q does not contain %q", err.Error(), "slug is empty")
			}
			if out != nil {
				t.Errorf("expected nil bytes, got %d bytes", len(out))
			}
		})
	}
}

func TestRender_ValidYAML(t *testing.T) {
	t.Parallel()
	out, err := Render("myproj")
	if err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(out, &m); err != nil {
		t.Fatalf("yaml.Unmarshal failed: %v\nrendered:\n%s", err, string(out))
	}
	for _, key := range []string{"AWSTemplateFormatVersion", "Resources", "Outputs"} {
		if _, ok := m[key]; !ok {
			t.Errorf("top-level key %q missing from rendered template", key)
		}
	}
	resources, ok := m["Resources"].(map[string]any)
	if !ok {
		t.Fatalf("Resources is not a map, got %T", m["Resources"])
	}
	if len(resources) == 0 {
		t.Errorf("Resources map is empty")
	}
}

func TestRender_ContainsSlug(t *testing.T) {
	t.Parallel()
	out, err := Render("myproj")
	if err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	s := string(out)
	expected := []string{
		"horde-myproj-vpc",
		"horde-myproj-public-1",
		"horde-myproj-private-2",
		"horde-myproj-nat",
		"horde-myproj-vpc-id",
		"horde-myproj",
		"/ecs/horde-worker-myproj",
	}
	for _, sub := range expected {
		if !strings.Contains(s, sub) {
			t.Errorf("rendered output missing %q", sub)
		}
	}
	if strings.Contains(s, "{{.Slug}}") {
		t.Errorf("rendered output contains unsubstituted {{.Slug}}")
	}
}

func TestRender_ResourcesPresent(t *testing.T) {
	t.Parallel()
	out, err := Render("myproj")
	if err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(out, &m); err != nil {
		t.Fatalf("yaml.Unmarshal failed: %v", err)
	}
	resources, ok := m["Resources"].(map[string]any)
	if !ok {
		t.Fatalf("Resources is not a map, got %T", m["Resources"])
	}

	expectedIDs := []string{
		"VPC",
		"InternetGateway",
		"InternetGatewayAttachment",
		"PublicSubnet1",
		"PublicSubnet2",
		"PrivateSubnet1",
		"PrivateSubnet2",
		"NatGatewayEIP",
		"NatGateway",
		"PublicRouteTable",
		"PublicDefaultRoute",
		"PublicSubnet1RouteAssoc",
		"PublicSubnet2RouteAssoc",
		"PrivateRouteTable",
		"PrivateDefaultRoute",
		"PrivateSubnet1RouteAssoc",
		"PrivateSubnet2RouteAssoc",
		"EcrRepository",
		"EcsCluster",
		"WorkerSecurityGroup",
		"TaskExecutionRole",
		"TaskRole",
		"LogGroup",
		"WorkerTaskDefinition",
		"RunsTable",
	}
	for _, id := range expectedIDs {
		id := id
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			if _, ok := resources[id]; !ok {
				t.Errorf("resource %q missing from rendered template", id)
			}
		})
	}
}

func TestRender_RunsTableGSIs(t *testing.T) {
	t.Parallel()
	out, err := Render("myproj")
	if err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(out, &m); err != nil {
		t.Fatalf("yaml.Unmarshal failed: %v", err)
	}
	resources := m["Resources"].(map[string]any)
	runs, ok := resources["RunsTable"].(map[string]any)
	if !ok {
		t.Fatalf("RunsTable is not a map, got %T", resources["RunsTable"])
	}
	props, ok := runs["Properties"].(map[string]any)
	if !ok {
		t.Fatalf("RunsTable.Properties is not a map, got %T", runs["Properties"])
	}
	if got := props["BillingMode"]; got != "PAY_PER_REQUEST" {
		t.Errorf("BillingMode = %v, want PAY_PER_REQUEST", got)
	}
	gsis, ok := props["GlobalSecondaryIndexes"].([]any)
	if !ok {
		t.Fatalf("GSI list is not a slice, got %T", props["GlobalSecondaryIndexes"])
	}
	wantIndexes := map[string]bool{"by-repo": false, "by-ticket": false, "by-status": false, "by-instance": false}
	for _, raw := range gsis {
		gsi, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("GSI entry is not a map, got %T", raw)
		}
		name, _ := gsi["IndexName"].(string)
		if _, want := wantIndexes[name]; want {
			wantIndexes[name] = true
		}
	}
	for name, seen := range wantIndexes {
		if !seen {
			t.Errorf("GSI %q missing", name)
		}
	}
}

func TestRender_OutputsPresent(t *testing.T) {
	t.Parallel()
	out, err := Render("myproj")
	if err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(out, &m); err != nil {
		t.Fatalf("yaml.Unmarshal failed: %v", err)
	}
	outputs, ok := m["Outputs"].(map[string]any)
	if !ok {
		t.Fatalf("Outputs is not a map, got %T", m["Outputs"])
	}

	expectedIDs := []string{
		"VpcId",
		"PublicSubnetIds",
		"PrivateSubnetIds",
		"ClusterArn",
		"TaskDefinitionArn",
		"WorkerSecurityGroupId",
		"EcrRepositoryUri",
		"LogGroupName",
		"RunsTableName",
	}
	for _, id := range expectedIDs {
		id := id
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			if _, ok := outputs[id]; !ok {
				t.Errorf("output %q missing from rendered template", id)
			}
		})
	}
}

func TestRender_CfnLint(t *testing.T) {
	t.Parallel()
	path, err := exec.LookPath("cfn-lint")
	if err != nil {
		t.Skip("cfn-lint not installed")
	}
	out, err := Render("myproj")
	if err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	tmpfile := filepath.Join(t.TempDir(), "stack.yaml")
	if err := os.WriteFile(tmpfile, out, 0o644); err != nil {
		t.Fatalf("writing temp file: %v", err)
	}
	cmd := exec.Command(path, tmpfile)
	combined, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cfn-lint failed:\n%s", combined)
	}
}
