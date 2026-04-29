package bootstrap

import (
	"bytes"
	_ "embed"
	"fmt"
	"strings"
	"text/template"

	"github.com/jorge-barreto/horde/internal/config"
)

//go:embed templates/stack.yaml.tmpl
var stackTemplateSource string

// Render generates a CloudFormation stack template for the given project
// slug. The slug is expected to come from Slug(remoteURL).
//
// extraSecrets are caller-declared aws-secret references beyond the two
// canonicals (CLAUDE_CODE_OAUTH_TOKEN, GIT_TOKEN). Each one becomes:
//
//   - an extra entry in the worker container's Secrets array (env name =
//     EnvName, ValueFrom = the looked-up Secrets Manager ARN)
//   - extra resources in the IAM Allow-Resource lists for both the
//     execution role's ResolveContainerSecrets and the task role's
//     SecretsRead policies
//
// The caller is responsible for ensuring the named Secrets Manager
// entries actually exist before deploy. Pass nil/empty for the v0.2
// canonical-only case (zero migration).
func Render(slug string, extraSecrets []config.ExtraAWSSecret) ([]byte, error) {
	if strings.TrimSpace(slug) == "" {
		return nil, fmt.Errorf("rendering stack template: slug is empty")
	}
	tmpl, err := template.New("stack").Option("missingkey=error").Parse(stackTemplateSource)
	if err != nil {
		return nil, fmt.Errorf("rendering stack template: parsing: %w", err)
	}
	var buf bytes.Buffer
	data := struct {
		Slug         string
		ExtraSecrets []config.ExtraAWSSecret
	}{
		Slug:         slug,
		ExtraSecrets: extraSecrets,
	}
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("rendering stack template: executing: %w", err)
	}
	return buf.Bytes(), nil
}
