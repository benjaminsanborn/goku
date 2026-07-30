package main

import (
	"fmt"
	"hash/fnv"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const manifestFile = "platform.yaml"

// manifest is parsed loosely so `platform add` round-trips fields it doesn't
// understand; the server-side schema validation is the strict gate.
type manifest struct {
	doc map[string]any
}

type resource struct {
	Name string
	Type string
}

func loadManifest() (*manifest, error) {
	b, err := os.ReadFile(manifestFile)
	if err != nil {
		return nil, fmt.Errorf("no %s here — run from a platform workspace root", manifestFile)
	}
	doc := map[string]any{}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", manifestFile, err)
	}
	return &manifest{doc: doc}, nil
}

func (m *manifest) save() error {
	b, err := yaml.Marshal(m.doc)
	if err != nil {
		return err
	}
	return os.WriteFile(manifestFile, b, 0o644)
}

func (m *manifest) resources() []resource {
	raw, _ := m.doc["resources"].(map[string]any)
	out := []resource{}
	for name, v := range raw {
		if spec, ok := v.(map[string]any); ok {
			if t, ok := spec["type"].(string); ok {
				out = append(out, resource{Name: name, Type: t})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (m *manifest) addResource(kind, name string) error {
	switch kind {
	case "database", "storage":
	default:
		return fmt.Errorf("unknown resource type %q (supported: database, storage)", kind)
	}
	res, ok := m.doc["resources"].(map[string]any)
	if !ok {
		res = map[string]any{}
		m.doc["resources"] = res
	}
	if _, exists := res[name]; exists {
		return fmt.Errorf("resource %q already in %s", name, manifestFile)
	}
	res[name] = map[string]any{"type": kind}
	return nil
}

// port derives a stable local port per (project, resource, facet) so cognates
// don't collide across projects and env vars survive restarts.
func port(parts ...string) int {
	h := fnv.New32a()
	h.Write([]byte(strings.Join(parts, "/")))
	return 20000 + int(h.Sum32()%10000)
}

func scaffoldManifest() string {
	return `# platform.yaml — declares what this project needs.
# The platform materializes these as AWS resources on merge; locally,
# 'platform dev' runs cognates (postgres, minio) with the same env contract.

services:
  api:
    type: api            # Fargate in prod; your Dockerfile locally
    size: small
    port: 8080
    health_check: /

resources: {}
  # Added via 'platform add database main' / 'platform add storage assets'

routes:
  - domain: default      # <project>.app.<platform-domain>
    service: api
`
}
