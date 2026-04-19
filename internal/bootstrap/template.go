package bootstrap

import (
	"bytes"
	_ "embed"
	"fmt"
	"strings"
	"text/template"
)

//go:embed templates/stack.yaml.tmpl
var stackTemplateSource string

// Render generates a CloudFormation stack template for the given project slug.
// The slug is expected to come from Slug(remoteURL).
func Render(slug string) ([]byte, error) {
	if strings.TrimSpace(slug) == "" {
		return nil, fmt.Errorf("rendering stack template: slug is empty")
	}
	tmpl, err := template.New("stack").Option("missingkey=error").Parse(stackTemplateSource)
	if err != nil {
		return nil, fmt.Errorf("rendering stack template: parsing: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, struct{ Slug string }{Slug: slug}); err != nil {
		return nil, fmt.Errorf("rendering stack template: executing: %w", err)
	}
	return buf.Bytes(), nil
}
