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

// A long-form ports entry (list-of-maps) — must survive untouched, never
// silently dropped, even though it isn't a remap candidate.
const fixtureLongFormPorts = `
services:
  api:
    image: some/api
    ports:
      - target: 8080
        published: 8080
        protocol: tcp
      - "9090:9090"
`

func writeComposeFixture(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func writeFixture(t *testing.T) string { t.Helper(); return writeComposeFixture(t, fixtureCompose) }

func TestRemapHandlesEnvDefaultPortSyntax(t *testing.T) {
	res, err := remapPublishedPorts(writeFixture(t), 10100, "trend-rider")
	if err != nil {
		t.Fatalf("remap: %v", err)
	}
	if len(res.Mappings) != 2 {
		t.Fatalf("want 2 remapped ports (the bare '6379' must be skipped), got %d: %+v", len(res.Mappings), res.Mappings)
	}
	byContainer := map[string]portMapping{}
	for _, m := range res.Mappings {
		byContainer[m.Service+":"+m.ContainerPort] = m
	}
	if m, ok := byContainer["backend:8080"]; !ok || m.NewHostPort != 10100 {
		t.Fatalf("expected backend:8080 -> 10100, got %+v (ok=%v)", m, ok)
	}
	if m, ok := byContainer["backend:50051"]; !ok || m.NewHostPort != 10101 {
		t.Fatalf("expected backend:50051 -> 10101, got %+v (ok=%v)", m, ok)
	}
}

// Regression test: a real bug caught by qa review — long-form port entries
// (list-of-maps syntax) aren't strings, so they were silently dropped
// entirely (svc["ports"] got overwritten with only the remapped entries).
func TestRemapPreservesLongFormPortsUnchanged(t *testing.T) {
	res, err := remapPublishedPorts(writeComposeFixture(t, fixtureLongFormPorts), 10100, "someproduct")
	if err != nil {
		t.Fatalf("remap: %v", err)
	}
	// Only the short-form "9090:9090" entry is a remap candidate.
	if len(res.Mappings) != 1 || res.Mappings[0].ContainerPort != "9090" {
		t.Fatalf("expected exactly one remap (9090), got %+v", res.Mappings)
	}
	svc := res.Doc["services"].(map[string]any)["api"].(map[string]any)
	ports := svc["ports"].([]any)
	if len(ports) != 2 {
		t.Fatalf("expected 2 port entries to survive (1 long-form untouched + 1 remapped), got %d: %+v", len(ports), ports)
	}
	foundLongForm := false
	for _, p := range ports {
		if m, ok := p.(map[string]any); ok {
			foundLongForm = true
			if m["target"] != 8080 {
				t.Fatalf("long-form entry was mutated: %+v", m)
			}
		}
	}
	if !foundLongForm {
		t.Fatalf("long-form port entry was dropped entirely, got %+v", ports)
	}
}

func TestRemapRenamesHardcodedNetwork(t *testing.T) {
	res, err := remapPublishedPorts(writeFixture(t), 10100, "trend-rider")
	if err != nil {
		t.Fatalf("remap: %v", err)
	}
	if !res.NetworkRenamed {
		t.Fatal("expected the network to be renamed")
	}
	nets := res.Doc["networks"].(map[string]any)
	def := nets["default"].(map[string]any)
	if def["name"] != "trend-rider-net" {
		t.Fatalf("expected trend-rider-net, got %v", def["name"])
	}
}

// This is the fix for the actual production incident: a product's health
// check must be reachable from the Factory over the internal Docker network
// alone — no published host port, no OCI Security List rule required.
func TestRemapAttachesFactoryNetworkAndSetsContainerNames(t *testing.T) {
	res, err := remapPublishedPorts(writeFixture(t), 10100, "trend-rider")
	if err != nil {
		t.Fatalf("remap: %v", err)
	}
	if res.ContainerNames["backend"] != "trend-rider-backend" {
		t.Fatalf("expected container name trend-rider-backend, got %q", res.ContainerNames["backend"])
	}
	if res.ContainerNames["cache"] != "trend-rider-cache" {
		t.Fatalf("expected container name trend-rider-cache, got %q", res.ContainerNames["cache"])
	}
	if len(res.Overridden) != 0 {
		t.Fatalf("fixture declares no pre-existing container_name, expected no overrides, got %v", res.Overridden)
	}

	nets := res.Doc["networks"].(map[string]any)
	fn, ok := nets[FactoryNetwork].(map[string]any)
	if !ok || fn["external"] != true {
		t.Fatalf("expected top-level networks.%s to be external:true, got %+v (ok=%v)", FactoryNetwork, fn, ok)
	}

	services := res.Doc["services"].(map[string]any)
	for _, name := range []string{"backend", "cache"} {
		svc := services[name].(map[string]any)
		if svc["container_name"] != res.ContainerNames[name] {
			t.Fatalf("service %s: container_name not set to %q, got %v", name, res.ContainerNames[name], svc["container_name"])
		}
		nlist, ok := svc["networks"].([]any)
		if !ok {
			t.Fatalf("service %s: networks should be a list, got %T", name, svc["networks"])
		}
		found := false
		for _, n := range nlist {
			if n == FactoryNetwork {
				found = true
			}
		}
		if !found {
			t.Fatalf("service %s: not attached to %s, got %v", name, FactoryNetwork, nlist)
		}
	}
}

// A repo that already declares its own container_name gets it overridden
// (so ours is always predictable/unique), but that override must be
// reported back — a peer service or external script referencing the
// original name directly would otherwise silently break.
func TestRemapReportsOverriddenContainerName(t *testing.T) {
	path := writeComposeFixture(t, "services:\n  api:\n    image: some/api\n    container_name: my-custom-name\n")
	res, err := remapPublishedPorts(path, 10100, "someproduct")
	if err != nil {
		t.Fatalf("remap: %v", err)
	}
	if len(res.Overridden) != 1 || res.Overridden[0] != "api" {
		t.Fatalf("expected api reported as overridden, got %v", res.Overridden)
	}
	svc := res.Doc["services"].(map[string]any)["api"].(map[string]any)
	if svc["container_name"] != "someproduct-api" {
		t.Fatalf("expected the override to still apply our own name, got %v", svc["container_name"])
	}
}

// Even a service with zero published ports must still get a container_name
// and factory-net attachment — the Factory may need to health-check it via
// an unpublished port too.
func TestRemapAttachesFactoryNetworkEvenWithNoPorts(t *testing.T) {
	path := writeComposeFixture(t, "services:\n  worker:\n    image: some/worker\n")
	res, err := remapPublishedPorts(path, 10100, "no-ports-product")
	if err != nil {
		t.Fatalf("remap: %v", err)
	}
	if res.ContainerNames["worker"] != "no-ports-product-worker" {
		t.Fatalf("expected container name, got %q", res.ContainerNames["worker"])
	}
	svc := res.Doc["services"].(map[string]any)["worker"].(map[string]any)
	nlist := svc["networks"].([]any)
	if len(nlist) != 2 || nlist[0] != "default" || nlist[1] != FactoryNetwork {
		t.Fatalf("expected [default, %s], got %v", FactoryNetwork, nlist)
	}
}

func TestRemapIsDeterministic(t *testing.T) {
	path := writeFixture(t)
	r1, err := remapPublishedPorts(path, 10100, "trend-rider")
	if err != nil {
		t.Fatalf("remap 1: %v", err)
	}
	r2, err := remapPublishedPorts(path, 10100, "trend-rider")
	if err != nil {
		t.Fatalf("remap 2: %v", err)
	}
	if r1.NetworkRenamed != r2.NetworkRenamed || len(r1.Mappings) != len(r2.Mappings) {
		t.Fatalf("non-deterministic across calls: %v/%d vs %v/%d", r1.NetworkRenamed, len(r1.Mappings), r2.NetworkRenamed, len(r2.Mappings))
	}
	for i := range r1.Mappings {
		if r1.Mappings[i] != r2.Mappings[i] {
			t.Fatalf("non-deterministic at index %d: %+v vs %+v", i, r1.Mappings[i], r2.Mappings[i])
		}
	}
	for svc, name := range r1.ContainerNames {
		if r2.ContainerNames[svc] != name {
			t.Fatalf("non-deterministic container name for %s: %q vs %q", svc, name, r2.ContainerNames[svc])
		}
	}
}

func TestRemapNoOpOnPortsWhenNoFixedPorts(t *testing.T) {
	path := writeComposeFixture(t, "services:\n  worker:\n    image: some/worker\n")
	res, err := remapPublishedPorts(path, 10100, "no-ports-product")
	if err != nil {
		t.Fatalf("remap: %v", err)
	}
	if len(res.Mappings) != 0 || res.NetworkRenamed {
		t.Fatalf("expected no port/network-rename changes, got mappings=%d renamed=%v", len(res.Mappings), res.NetworkRenamed)
	}
}

// Confirms the generated file is well-formed YAML a compose parser could
// consume — full docker-compose CLI validation was run manually against
// this fixture and Market-AI's real file during development (see PR notes).
func TestWriteFactoryComposeProducesValidYAML(t *testing.T) {
	res, err := remapPublishedPorts(writeFixture(t), 10100, "trend-rider")
	if err != nil {
		t.Fatalf("remap: %v", err)
	}
	dir := t.TempDir()
	if err := writeFactoryCompose(dir, res.Doc); err != nil {
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

// Mirrors the real nof1.ai onboarding failure: mem_limit/memswap_limit both
// set to 200m, below the floor, gets OOM-killed even with 21GB free on the
// host — both must be raised together, since Docker rejects
// memswap_limit < mem_limit.
func TestRemapRaisesMemoryLimitBelowFloor(t *testing.T) {
	path := writeComposeFixture(t, "services:\n  app:\n    image: some/app\n    mem_limit: 200m\n    memswap_limit: 200m\n")
	res, err := remapPublishedPorts(path, 10100, "nof1-ai")
	if err != nil {
		t.Fatalf("remap: %v", err)
	}
	svc := res.Doc["services"].(map[string]any)["app"].(map[string]any)
	if svc["mem_limit"] != memoryFloorBytes {
		t.Fatalf("expected mem_limit raised to floor %d, got %v", memoryFloorBytes, svc["mem_limit"])
	}
	if svc["memswap_limit"] != memoryFloorBytes {
		t.Fatalf("expected memswap_limit raised to match, got %v", svc["memswap_limit"])
	}
}

// A product that already provisioned generously must be left alone — the
// floor only raises, it never second-guesses a limit that already clears it.
func TestRemapDoesNotLowerMemoryLimitAboveFloor(t *testing.T) {
	path := writeComposeFixture(t, "services:\n  app:\n    image: some/app\n    mem_limit: 2g\n")
	res, err := remapPublishedPorts(path, 10100, "generous-product")
	if err != nil {
		t.Fatalf("remap: %v", err)
	}
	svc := res.Doc["services"].(map[string]any)["app"].(map[string]any)
	if svc["mem_limit"] != "2g" {
		t.Fatalf("expected mem_limit left untouched at 2g, got %v", svc["mem_limit"])
	}
}

// Most repos declare no mem_limit at all — unlimited already clears any
// floor, so the floor must never introduce a cap where the author set none.
func TestRemapDoesNotAddMemoryLimitWhenAbsent(t *testing.T) {
	path := writeComposeFixture(t, "services:\n  app:\n    image: some/app\n")
	res, err := remapPublishedPorts(path, 10100, "no-limit-product")
	if err != nil {
		t.Fatalf("remap: %v", err)
	}
	svc := res.Doc["services"].(map[string]any)["app"].(map[string]any)
	if _, ok := svc["mem_limit"]; ok {
		t.Fatalf("expected no mem_limit to be introduced, got %v", svc["mem_limit"])
	}
}

func TestParseMemBytes(t *testing.T) {
	cases := []struct {
		in     any
		want   int64
		wantOK bool
	}{
		{"200m", 200 * 1024 * 1024, true},
		{"1g", 1024 * 1024 * 1024, true},
		{"512mb", 512 * 1024 * 1024, true},
		{"1024k", 1024 * 1024, true},
		{"209715200", 209715200, true},
		{209715200, 209715200, true},
		{float64(209715200), 209715200, true},
		{"not-a-limit", 0, false},
		{nil, 0, false},
	}
	for _, c := range cases {
		got, ok := parseMemBytes(c.in)
		if ok != c.wantOK || (ok && got != c.want) {
			t.Errorf("parseMemBytes(%v) = (%d, %v), want (%d, %v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}
