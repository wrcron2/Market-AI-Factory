package wizard

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/wrcron2/market-ai-factory/backend/internal/orchestrator"
)

// FactoryNetwork is the shared Docker network the Factory's own containers
// run on (infra/docker-compose.factory.yml). Every product service is also
// attached to it so the Factory can reach the product directly by container
// name over Docker's internal DNS — no published host port, no cloud
// firewall rule, works the instant the container starts. Health checks going
// out over the public internet (and needing an OCI Security List rule per
// product's port range) was the wrong design; this is the fix. Canonically
// defined in the orchestrator package, which also knows how to ensure it
// exists before a deploy.
const FactoryNetwork = orchestrator.FactoryNetwork

// portMapping describes one published port after remapping.
type portMapping struct {
	Service       string
	ContainerPort string
	Proto         string
	NewHostPort   int
}

// remapResult bundles everything a product's compose file needed changed
// (ports, network name, container names) plus the fully-rewritten doc.
type remapResult struct {
	Mappings       []portMapping
	ContainerNames map[string]string // service -> assigned container_name
	NetworkRenamed bool
	// Overridden lists services where the repo already declared its own
	// container_name that we replaced — surfaced so the caller can log it;
	// a peer service or external script referencing the original name would
	// silently break, since Docker only aliases the name we actually set.
	Overridden []string
	Doc        map[string]any
}

// containerPortRe matches the container-port side of a compose ports: entry,
// regardless of how the host side is written — a literal ("8080:8080"),
// IP-bound ("127.0.0.1:8080:8080"), or an interpolated env var with a
// default ("${GO_SERVER_PORT:-8080}:8080", exactly what Market-AI's own
// compose file uses). Anchoring to the end of the string means we don't
// care what precedes the last colon — we only need the container port,
// because the host side gets replaced outright, never merged.
var containerPortRe = regexp.MustCompile(`:(\d+)(?:/(tcp|udp))?$`)

// memoryFloorBytes is the minimum mem_limit the Factory enforces on every
// onboarded product. Some repos under-provision for whatever host they were
// authored against and get OOM-killed here even with headroom to spare — the
// nof1.ai onboarding hit this exactly: its own docker-compose.yml sets
// mem_limit: 200m, and the kernel killed it within seconds of every start
// (dmesg: "Memory cgroup out of memory") despite 21GB free on the box. Only
// ever raises an explicit limit below this floor — never lowers one, and
// never adds a limit where none existed (unlimited already clears any floor).
const memoryFloorBytes int64 = 512 * 1024 * 1024 // 512m

// memUnitRe parses a compose mem_limit-style value: a bare byte count, or a
// number followed by an optional b/k/m/g unit (with an optional trailing
// "b", e.g. "512mb") — the forms docker-compose itself accepts.
var memUnitRe = regexp.MustCompile(`(?i)^(\d+)\s*([bkmg]?)b?$`)

// parseMemBytes converts a compose mem_limit/memswap_limit value (YAML may
// hand back an int, float64, or string depending on how it was written) into
// bytes. ok is false for anything it doesn't recognize — callers must leave
// those values untouched rather than guess and risk corrupting a value the
// product author set deliberately.
func parseMemBytes(v any) (bytes int64, ok bool) {
	switch t := v.(type) {
	case int:
		return int64(t), true
	case int64:
		return t, true
	case float64:
		return int64(t), true
	case string:
		m := memUnitRe.FindStringSubmatch(strings.TrimSpace(t))
		if m == nil {
			return 0, false
		}
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return 0, false
		}
		switch strings.ToLower(m[2]) {
		case "k":
			return n * 1024, true
		case "m":
			return n * 1024 * 1024, true
		case "g":
			return n * 1024 * 1024 * 1024, true
		default:
			return n, true
		}
	default:
		return 0, false
	}
}

// raiseMemoryFloor enforces memoryFloorBytes on a service's mem_limit, and
// keeps memswap_limit in step — Docker rejects memswap_limit < mem_limit, so
// a paired limit below the floor is raised to match rather than left to
// break compose up outright.
func raiseMemoryFloor(svc map[string]any) {
	raw, ok := svc["mem_limit"]
	if !ok {
		return // no limit declared — unlimited already satisfies any floor
	}
	b, ok := parseMemBytes(raw)
	if !ok || b >= memoryFloorBytes {
		return
	}
	svc["mem_limit"] = memoryFloorBytes
	if swapRaw, ok := svc["memswap_limit"]; ok {
		if swapB, ok := parseMemBytes(swapRaw); ok && swapB < memoryFloorBytes {
			svc["memswap_limit"] = memoryFloorBytes
		}
	}
}

func extractContainerPort(entry string) (port, proto string, ok bool) {
	if !strings.Contains(entry, ":") {
		return "", "", false // bare "6379" — no fixed host port, nothing to remap
	}
	m := containerPortRe.FindStringSubmatch(entry)
	if m == nil {
		return "", "", false // unrecognized / long-form (list-of-maps) entry — left alone
	}
	return m[1], m[2], true
}

// remapPublishedPorts parses a product's docker-compose.yml and:
//  1. reassigns every published (string-form) container port to a fresh
//     sequential host port starting at base — deterministically (sorted
//     service names, sorted port entries), since the wizard may re-run this
//     on retry. Long-form (list-of-maps) port entries are preserved
//     UNCHANGED, never dropped — they aren't remapped, but they must survive;
//  2. renames an explicit top-level `networks.default.name` (if any) to
//     <productSlug>-net;
//  3. gives every service a stable, globally-unique container_name
//     (<productSlug>-<service>) and attaches it to the shared FactoryNetwork
//     alongside its own private network;
//  4. raises any mem_limit below memoryFloorBytes up to the floor (never
//     lowers one, never adds one where none existed).
//
// (1) and (2) are real collisions, not hypothetical: Market-AI's own compose
// file hardcodes both its ports and its network name ("marketflow-net"), so
// any product forked from a similarly-shaped repo would otherwise fight over
// one or the other. (3) is what makes the product reachable by the Factory
// for health checks without a published host port or any cloud firewall
// rule — internal Docker DNS, live the instant the container starts. (4) is
// real too, not hypothetical: nof1.ai's own compose file caps itself at
// 200m and gets OOM-killed on this box within seconds of every start, despite
// 21GB free — see memoryFloorBytes.
//
// The doc is rewritten wholesale (not layered as a docker-compose.override.yml)
// because compose CONCATENATES `ports:` lists across -f files rather than
// replacing them — an override wouldn't remove the original colliding
// binding, only add a second one alongside it.
func remapPublishedPorts(composeFile string, base int, productSlug string) (*remapResult, error) {
	raw, err := os.ReadFile(composeFile)
	if err != nil {
		return nil, fmt.Errorf("read compose file: %w", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse compose file: %w", err)
	}

	res := &remapResult{ContainerNames: map[string]string{}, Doc: doc}

	nets, ok := doc["networks"].(map[string]any)
	if !ok {
		nets = map[string]any{}
	}
	if def, ok := nets["default"].(map[string]any); ok {
		if _, hasName := def["name"]; hasName {
			def["name"] = productSlug + "-net"
			nets["default"] = def
			res.NetworkRenamed = true
		}
	}
	nets[FactoryNetwork] = map[string]any{"external": true}
	doc["networks"] = nets

	services, _ := doc["services"].(map[string]any)
	if len(services) == 0 {
		return res, nil
	}

	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names) // determinism — Check() may re-derive this on retry

	next := base
	for _, name := range names {
		svc, _ := services[name].(map[string]any)

		if orig, ok := svc["container_name"]; ok && orig != "" {
			res.Overridden = append(res.Overridden, name)
		}
		containerName := productSlug + "-" + name
		res.ContainerNames[name] = containerName
		svc["container_name"] = containerName
		svc["networks"] = attachFactoryNetwork(svc["networks"])
		raiseMemoryFloor(svc)

		portsRaw, _ := svc["ports"].([]any)
		if len(portsRaw) > 0 {
			// Long-form entries (list-of-maps, e.g. {target:, published:,
			// protocol:}) aren't strings — they must be preserved untouched,
			// never dropped. Only string (short-form) entries are sorted and
			// candidates for remapping.
			var stringEntries []string
			var otherEntries []any
			for _, p := range portsRaw {
				if s, ok := p.(string); ok {
					stringEntries = append(stringEntries, s)
				} else {
					otherEntries = append(otherEntries, p)
				}
			}
			sort.Strings(stringEntries)

			newEntries := make([]any, 0, len(stringEntries)+len(otherEntries))
			for _, entry := range stringEntries {
				containerPort, proto, ok := extractContainerPort(entry)
				if !ok {
					newEntries = append(newEntries, entry)
					continue
				}
				newHost := next
				next++
				res.Mappings = append(res.Mappings, portMapping{Service: name, ContainerPort: containerPort, Proto: proto, NewHostPort: newHost})
				if proto != "" {
					newEntries = append(newEntries, fmt.Sprintf("%d:%s/%s", newHost, containerPort, proto))
				} else {
					newEntries = append(newEntries, fmt.Sprintf("%d:%s", newHost, containerPort))
				}
			}
			newEntries = append(newEntries, otherEntries...)
			svc["ports"] = newEntries
		}
		services[name] = svc
	}
	doc["services"] = services
	return res, nil
}

// attachFactoryNetwork adds FactoryNetwork to a service's networks list,
// preserving whatever was there before. Compose service networks may be a
// plain list of names or a map (name -> per-network settings like aliases);
// once a service declares an explicit networks: key at all, compose stops
// implicitly joining "default", so "default" must be added back explicitly
// when nothing was declared before.
func attachFactoryNetwork(existing any) any {
	switch v := existing.(type) {
	case []any:
		for _, n := range v {
			if s, ok := n.(string); ok && s == FactoryNetwork {
				return v
			}
		}
		return append(v, FactoryNetwork)
	case map[string]any:
		if _, ok := v[FactoryNetwork]; !ok {
			v[FactoryNetwork] = nil
		}
		return v
	default:
		return []any{"default", FactoryNetwork}
	}
}

// writeFactoryCompose writes the fully-resolved, port-remapped doc to
// docker-compose.factory.yml — the product's own docker-compose.yml is
// never modified.
func writeFactoryCompose(dir string, doc map[string]any) error {
	out, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	return os.WriteFile(dir+"/docker-compose.factory.yml", out, 0o644)
}

// writePortsInfo writes a human-readable products/<name>/ports.yaml so
// whoever fills in health_url during onboarding knows both addresses: the
// internal one (works immediately, no firewall) and the host port (for a
// human hitting it from a browser).
func writePortsInfo(repoRoot, productName string, mappings []portMapping, containerNames map[string]string) error {
	dir := repoRoot + "/products/" + productName
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	lines := "# Addresses this product's services are reachable at, written by the\n" +
		"# deploy step. internal_url works from inside the Factory network right\n" +
		"# away (use this for health_url); host_port is for a human's browser.\n"
	for _, m := range mappings {
		lines += fmt.Sprintf("%s: {internal_url: \"http://%s:%s\", host_port: %d}\n",
			m.Service, containerNames[m.Service], m.ContainerPort, m.NewHostPort)
	}
	return os.WriteFile(dir+"/ports.yaml", []byte(lines), 0o644)
}
