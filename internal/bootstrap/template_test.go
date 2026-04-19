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
		"horde-myproj-anthropic-api-key",
		"horde-myproj-git-token",
		"horde-artifacts-myproj-",
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
		"AnthropicApiKeySecret",
		"GitTokenSecret",
		"ArtifactsBucket",
		"ArtifactsBucketPolicy",
		"CliUserManagedPolicy",
		"HordeConfigParameter",
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
		"ArtifactsBucketName",
		"AnthropicApiKeySecretArn",
		"GitTokenSecretArn",
		"CliUserManagedPolicyArn",
		"SsmConfigPath",
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

func TestRender_ParametersPresent(t *testing.T) {
	t.Parallel()
	out, err := Render("myproj")
	if err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(out, &m); err != nil {
		t.Fatalf("yaml.Unmarshal failed: %v", err)
	}
	params, ok := m["Parameters"].(map[string]any)
	if !ok {
		t.Fatalf("Parameters is not a map, got %T", m["Parameters"])
	}
	for _, name := range []string{"AnthropicApiKey", "GitToken"} {
		entry, ok := params[name].(map[string]any)
		if !ok {
			t.Errorf("parameter %q missing or wrong shape, got %T", name, params[name])
			continue
		}
		if ne, _ := entry["NoEcho"].(bool); !ne {
			t.Errorf("parameter %q: NoEcho = %v, want true", name, entry["NoEcho"])
		}
	}
}

func TestRender_TaskDefSecrets(t *testing.T) {
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
	td, ok := resources["WorkerTaskDefinition"].(map[string]any)
	if !ok {
		t.Fatalf("WorkerTaskDefinition is not a map, got %T", resources["WorkerTaskDefinition"])
	}
	props, ok := td["Properties"].(map[string]any)
	if !ok {
		t.Fatalf("WorkerTaskDefinition.Properties is not a map, got %T", td["Properties"])
	}
	containers, ok := props["ContainerDefinitions"].([]any)
	if !ok || len(containers) == 0 {
		t.Fatalf("ContainerDefinitions missing or empty: %T", props["ContainerDefinitions"])
	}
	container, ok := containers[0].(map[string]any)
	if !ok {
		t.Fatalf("ContainerDefinitions[0] is not a map, got %T", containers[0])
	}
	secrets, ok := container["Secrets"].([]any)
	if !ok {
		t.Fatalf("Secrets is not a slice, got %T", container["Secrets"])
	}
	if len(secrets) != 2 {
		t.Fatalf("expected 2 secrets, got %d", len(secrets))
	}
	wantRefs := map[string]bool{"AnthropicApiKeySecret": false, "GitTokenSecret": false}
	for _, raw := range secrets {
		s, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("secret entry is not a map, got %T", raw)
		}
		vf, ok := s["ValueFrom"].(map[string]any)
		if !ok {
			t.Fatalf("ValueFrom is not a map, got %T", s["ValueFrom"])
		}
		ref, _ := vf["Ref"].(string)
		if _, want := wantRefs[ref]; want {
			wantRefs[ref] = true
		}
	}
	for name, seen := range wantRefs {
		if !seen {
			t.Errorf("secret referencing %q missing", name)
		}
	}
}

func TestRender_TaskRoleHasPolicies(t *testing.T) {
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
	role, ok := resources["TaskRole"].(map[string]any)
	if !ok {
		t.Fatalf("TaskRole is not a map, got %T", resources["TaskRole"])
	}
	props, ok := role["Properties"].(map[string]any)
	if !ok {
		t.Fatalf("TaskRole.Properties is not a map, got %T", role["Properties"])
	}
	policies, ok := props["Policies"].([]any)
	if !ok {
		t.Fatalf("TaskRole.Policies is not a slice, got %T", props["Policies"])
	}
	if len(policies) < 1 {
		t.Fatalf("expected at least 1 inline policy on TaskRole, got %d", len(policies))
	}
	first, ok := policies[0].(map[string]any)
	if !ok {
		t.Fatalf("TaskRole.Policies[0] is not a map, got %T", policies[0])
	}
	doc, ok := first["PolicyDocument"].(map[string]any)
	if !ok {
		t.Fatalf("TaskRole.Policies[0].PolicyDocument is not a map, got %T", first["PolicyDocument"])
	}
	statements, ok := doc["Statement"].([]any)
	if !ok {
		t.Fatalf("TaskRole.Policies[0].PolicyDocument.Statement is not a slice, got %T", doc["Statement"])
	}
	wantSids := map[string]bool{"ArtifactsWrite": false, "SecretsRead": false}
	for _, raw := range statements {
		st, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("statement is not a map, got %T", raw)
		}
		sid, _ := st["Sid"].(string)
		if _, want := wantSids[sid]; want {
			wantSids[sid] = true
		}
	}
	for sid, seen := range wantSids {
		if !seen {
			t.Errorf("TaskRole inline policy missing Sid %q", sid)
		}
	}
}

func TestRender_CliPolicyStatements(t *testing.T) {
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
	policy, ok := resources["CliUserManagedPolicy"].(map[string]any)
	if !ok {
		t.Fatalf("CliUserManagedPolicy is not a map, got %T", resources["CliUserManagedPolicy"])
	}
	props, ok := policy["Properties"].(map[string]any)
	if !ok {
		t.Fatalf("CliUserManagedPolicy.Properties is not a map, got %T", policy["Properties"])
	}
	doc, ok := props["PolicyDocument"].(map[string]any)
	if !ok {
		t.Fatalf("CliUserManagedPolicy.Properties.PolicyDocument is not a map, got %T", props["PolicyDocument"])
	}
	statements, ok := doc["Statement"].([]any)
	if !ok {
		t.Fatalf("CliUserManagedPolicy PolicyDocument.Statement is not a slice, got %T", doc["Statement"])
	}
	wantSids := map[string]bool{
		"SsmRead":         false,
		"EcsRun":          false,
		"EcsPassRole":     false,
		"DynamoRunsTable": false,
		"LogsRead":        false,
		"ArtifactsRead":   false,
	}
	for _, raw := range statements {
		st, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("statement is not a map, got %T", raw)
		}
		sid, _ := st["Sid"].(string)
		if _, want := wantSids[sid]; want {
			wantSids[sid] = true
		}
	}
	for sid, seen := range wantSids {
		if !seen {
			t.Errorf("CliUserManagedPolicy missing statement with Sid %q", sid)
		}
	}
}

func TestRender_SsmConfigParameter(t *testing.T) {
	t.Parallel()
	out, err := Render("myproj")
	if err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `Name: "/horde/myproj/config"`) {
		t.Errorf("SSM parameter name not found in rendered output")
	}
	for _, key := range []string{
		"cluster_arn", "task_definition_arn", "subnets", "security_group",
		"assign_public_ip", "log_group", "log_stream_prefix", "artifacts_bucket",
		"runs_table", "ecr_repo_uri", "max_concurrent", "default_timeout_minutes",
	} {
		if !strings.Contains(s, `"`+key+`"`) {
			t.Errorf("SSM parameter JSON missing key %q", key)
		}
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
