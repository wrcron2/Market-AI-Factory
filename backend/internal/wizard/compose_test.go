package wizard

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// This fixture mirrors the real shape found in Market-AI's own
// docker-compose.yml (env-var host port with a default, a hardcoded network
// name) plus a bare container-only port, which must be left alone since
// docker already assigns that one an ephemeral host port.
const fixtureCompose = `
services:
  backend:
    image: some/backend
    ports:
      - "${GO_SERVER_PORT:-8080}:8080"
      - "50051:50051"
  cache:
    image: redis
    ports:
      - "6379"
networks:
  default:
    name: marketflow-net
`

func writeFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(path, []byte(fixtureCompose), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestRemapHandlesEnvDefaultPortSyntax(t *testing.T) {
	mappings, _, _, err := remapPublishedPorts(writeFixture(t), 10100, "trend-rider")
	if err != nil {
		t.Fatalf("remap: %v", err)
	}
	if len(mappings) != 2 {
		t.Fatalf("want 2 remapped ports (the bare '6379' must be skipped), got %d: %+v", len(mappings), mappings)
	}
	byContainer := map[string]portMapping{}
	for _, m := range mappings {
		byContainer[m.Service+":"+m.ContainerPort] = m
	}
	if m, ok := byContainer["backend:8080"]; !ok || m.NewHostPort != 10100 {
		t.Fatalf("expected backend:8080 -> 10100, got %+v (ok=%v)", m, ok)
	}
	if m, ok := byContainer["backend:50051"]; !ok || m.NewHostPort != 10101 {
		t.Fatalf("expected backend:50051 -> 10101, got %+v (ok=%v)", m, ok)
	}
}

func TestRemapRenamesHardcodedNetwork(t *testing.T) {
	_, renamed, doc, err := remapPublishedPorts(writeFixture(t), 10100, "trend-rider")
	if err != nil {
		t.Fatalf("remap: %v", err)
	}
	if !renamed {
		t.Fatal("expected the network to be renamed")
	}
	nets := doc["networks"].(map[string]any)
	def := nets["default"].(map[string]any)
	if def["name"] != "trend-rider-net" {
		t.Fatalf("expected trend-rider-net, got %v", def["name"])
	}
}

func TestRemapIsDeterministic(t *testing.T) {
	path := writeFixture(t)
	m1, r1, _, err := remapPublishedPorts(path, 10100, "trend-rider")
	if err != nil {
		t.Fatalf("remap 1: %v", err)
	}
	m2, r2, _, err := remapPublishedPorts(path, 10100, "trend-rider")
	if err != nil {
		t.Fatalf("remap 2: %v", err)
	}
	if r1 != r2 || len(m1) != len(m2) {
		t.Fatalf("non-deterministic across calls: %v/%d vs %v/%d", r1, len(m1), r2, len(m2))
	}
	for i := range m1 {
		if m1[i] != m2[i] {
			t.Fatalf("non-deterministic at index %d: %+v vs %+v", i, m1[i], m2[i])
		}
	}
}

func TestRemapNoOpWhenNoFixedPorts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "docker-compose.yml")
	os.WriteFile(path, []byte("services:\n  worker:\n    image: some/worker\n"), 0o644)
	mappings, renamed, _, err := remapPublishedPorts(path, 10100, "no-ports-product")
	if err != nil {
		t.Fatalf("remap: %v", err)
	}
	if len(mappings) != 0 || renamed {
		t.Fatalf("expected no changes for a compose file with no ports/network, got mappings=%d renamed=%v", len(mappings), renamed)
	}
}

// Confirms the generated file is well-formed YAML a compose parser could
// consume — full docker-compose CLI validation was run manually against
// this fixture and Market-AI's real file during development (see PR notes).
func TestWriteFactoryComposeProducesValidYAML(t *testing.T) {
	_, _, doc, err := remapPublishedPorts(writeFixture(t), 10100, "trend-rider")
	if err != nil {
		t.Fatalf("remap: %v", err)
	}
	dir := t.TempDir()
	if err := writeFactoryCompose(dir, doc); err != nil {
		t.Fatalf("write: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "docker-compose.factory.yml"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var reparsed map[string]any
	if err := yaml.Unmarshal(raw, &reparsed); err != nil {
		t.Fatalf("generated file is not valid YAML: %v", err)
	}
}
