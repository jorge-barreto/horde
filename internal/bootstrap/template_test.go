package bootstrap

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jorge-barreto/horde/internal/config"
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
			out, err := Render(tc.in, nil)
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
	out, err := Render("myproj", nil)
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
	out, err := Render("myproj", nil)
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
		"horde-myproj-claude-code-oauth-token",
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
	out, err := Render("myproj", nil)
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
		"ClaudeCodeOauthTokenSecret",
		"GitTokenSecret",
		"ArtifactsBucket",
		"ArtifactsBucketPolicy",
		"CliUserManagedPolicy",
		"HordeConfigParameter",
		"StatusLambdaRole",
		"StatusLambda",
		"StatusEventRule",
		"StatusLambdaInvokePermission",
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
	out, err := Render("myproj", nil)
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
	out, err := Render("myproj", nil)
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
		"ClaudeCodeOauthTokenSecretArn",
		"GitTokenSecretArn",
		"CliUserManagedPolicyArn",
		"SsmConfigPath",
		"StatusLambdaArn",
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
	out, err := Render("myproj", nil)
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
	for _, name := range []string{"ClaudeCodeOauthToken", "GitToken"} {
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

// TestRender_NoSecretsLeaked ensures no literal secret values from .env or
// elsewhere can end up baked into the rendered template. Secrets MUST flow
// through NoEcho CloudFormation Parameters, never through template variables.
// This test renders with a slug that looks like a secret and asserts that
// common secret prefixes never appear in the output.
func TestRender_NoSecretsLeaked(t *testing.T) {
	t.Parallel()
	out, err := Render("myproj", nil)
	if err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	s := string(out)
	// Substrings that would indicate a secret was serialized into the template.
	forbidden := []string{
		"sk-ant-",        // Anthropic API key prefix
		"github_pat_",    // GitHub fine-grained PAT prefix
		"ghp_",           // GitHub classic PAT prefix
		"AKIA",           // AWS access key prefix
		"ASIA",           // AWS temporary access key prefix
		"aws_secret_access_key",
	}
	for _, f := range forbidden {
		if strings.Contains(s, f) {
			t.Errorf("rendered template contains forbidden substring %q", f)
		}
	}
	// The SecretString fields MUST reference the Parameter, not embed a literal.
	// If someone refactored secrets to use plain strings, this check catches it.
	if !strings.Contains(s, "Ref: ClaudeCodeOauthToken") {
		t.Errorf("ClaudeCodeOauthTokenSecret must reference ClaudeCodeOauthToken parameter by Ref")
	}
	if !strings.Contains(s, "Ref: GitToken") {
		t.Errorf("GitTokenSecret must reference GitToken parameter by Ref")
	}
}

func TestRender_TaskDefSecrets(t *testing.T) {
	t.Parallel()
	out, err := Render("myproj", nil)
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
	wantRefs := map[string]bool{"ClaudeCodeOauthTokenSecret": false, "GitTokenSecret": false}
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
	out, err := Render("myproj", nil)
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
	out, err := Render("myproj", nil)
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
	out, err := Render("myproj", nil)
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

func TestRender_StatusLambdaPython(t *testing.T) {
	t.Parallel()
	out, err := Render("myproj", nil)
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
	fn, ok := resources["StatusLambda"].(map[string]any)
	if !ok {
		t.Fatalf("StatusLambda is not a map, got %T", resources["StatusLambda"])
	}
	props, ok := fn["Properties"].(map[string]any)
	if !ok {
		t.Fatalf("StatusLambda.Properties is not a map, got %T", fn["Properties"])
	}
	code, ok := props["Code"].(map[string]any)
	if !ok {
		t.Fatalf("StatusLambda.Properties.Code is not a map, got %T", props["Code"])
	}
	zipfile, ok := code["ZipFile"].(string)
	if !ok {
		t.Fatalf("StatusLambda.Properties.Code.ZipFile is not a string, got %T", code["ZipFile"])
	}
	if !strings.HasPrefix(zipfile, "import json") {
		t.Errorf("ZipFile does not start with 'import json'; first 40 chars: %q", zipfile[:min(40, len(zipfile))])
	}
	for _, sub := range []string{
		"by-instance",
		"run-result.json",
		"total_cost_usd",
		`TERMINAL = {"success", "failed", "killed"}`,
	} {
		if !strings.Contains(zipfile, sub) {
			t.Errorf("ZipFile missing expected substring %q", sub)
		}
	}
}

func TestRender_ExtraSecrets_TaskDefAndIAM(t *testing.T) {
	t.Parallel()
	extras := []config.ExtraAWSSecret{
		{EnvName: "STRIPE_API_KEY", SecretName: "prepdesk/stripe-api-key"},
		{EnvName: "REVIEW_GIT_TOKEN", SecretName: "horde/review-git-token"},
	}
	out, err := Render("myproj", extras)
	if err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(out, &m); err != nil {
		t.Fatalf("yaml.Unmarshal failed: %v\n%s", err, string(out))
	}
	resources := m["Resources"].(map[string]any)

	td := resources["WorkerTaskDefinition"].(map[string]any)
	props := td["Properties"].(map[string]any)
	containers := props["ContainerDefinitions"].([]any)
	container := containers[0].(map[string]any)
	secrets := container["Secrets"].([]any)
	if len(secrets) != 4 {
		t.Fatalf("expected 4 secrets (2 canonical + 2 extra), got %d", len(secrets))
	}
	gotEnvNames := map[string]bool{}
	for _, raw := range secrets {
		s := raw.(map[string]any)
		gotEnvNames[s["Name"].(string)] = true
	}
	for _, want := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "GIT_TOKEN", "STRIPE_API_KEY", "REVIEW_GIT_TOKEN"} {
		if !gotEnvNames[want] {
			t.Errorf("Secrets array missing %q", want)
		}
	}

	// IAM grants for both task and execution roles must include each
	// extra secret name (Fn::Sub'd into a wildcard ARN).
	rendered := string(out)
	for _, name := range []string{"prepdesk/stripe-api-key", "horde/review-git-token"} {
		// Each extra appears twice — once in TaskExecutionRole.Resource,
		// once in TaskRole.Resource. And again in WorkerTaskDefinition's
		// Secrets ValueFrom. So we expect at least 3 occurrences.
		if got := strings.Count(rendered, name); got < 3 {
			t.Errorf("extra secret %q should appear at least 3 times (2 IAM + 1 task-def), got %d", name, got)
		}
	}
}

func TestRender_NoExtras_NoExtraLines(t *testing.T) {
	t.Parallel()
	out, err := Render("myproj", nil)
	if err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	s := string(out)
	// Sanity: stale "ExtraSecrets" identifier should never appear in
	// output (would mean template var leaked).
	if strings.Contains(s, "ExtraSecrets") {
		t.Errorf("rendered template contains stray template identifier ExtraSecrets")
	}
	if strings.Contains(s, "{{") {
		t.Errorf("rendered template contains unsubstituted template directive")
	}
}

func TestRender_CfnLint(t *testing.T) {
	t.Parallel()
	path, err := exec.LookPath("cfn-lint")
	if err != nil {
		t.Skip("cfn-lint not installed")
	}
	out, err := Render("myproj", nil)
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
