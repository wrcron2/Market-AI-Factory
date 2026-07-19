package wizard

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// portMapping describes one published port after remapping.
type portMapping struct {
	Service       string
	ContainerPort string
	Proto         string
	NewHostPort   int
}

// containerPortRe matches the container-port side of a compose ports: entry,
// regardless of how the host side is written — a literal ("8080:8080"),
// IP-bound ("127.0.0.1:8080:8080"), or an interpolated env var with a
// default ("${GO_SERVER_PORT:-8080}:8080", exactly what Market-AI's own
// compose file uses). Anchoring to the end of the string means we don't
// care what precedes the last colon — we only need the container port,
// because the host side gets replaced outright, never merged.
var containerPortRe = regexp.MustCompile(`:(\d+)(?:/(tcp|udp))?$`)

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

// remapPublishedPorts parses a product's docker-compose.yml, reassigns every
// published container port to a fresh sequential host port starting at base
// — deterministically (sorted service names, sorted port entries), since the
// wizard may re-run this on retry — and renames an explicit top-level
// `networks.default.name` (if any) to <productSlug>-net. Both are real
// collisions, not hypothetical: Market-AI's own compose file hardcodes both
// its ports and its network name ("marketflow-net"), so any product forked
// from a similarly-shaped repo would otherwise fight over one or the other.
// Returns the port mappings, whether the network was renamed, and the FULL
// compose doc with those fields rewritten in place.
//
// The doc is rewritten wholesale (not layered as a docker-compose.override.yml)
// because compose CONCATENATES `ports:` lists across -f files rather than
// replacing them — an override wouldn't remove the original colliding
// binding, only add a second one alongside it.
func remapPublishedPorts(composeFile string, base int, productSlug string) ([]portMapping, bool, map[string]any, error) {
	raw, err := os.ReadFile(composeFile)
	if err != nil {
		return nil, false, nil, fmt.Errorf("read compose file: %w", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, false, nil, fmt.Errorf("parse compose file: %w", err)
	}

	networkRenamed := false
	if nets, ok := doc["networks"].(map[string]any); ok {
		if def, ok := nets["default"].(map[string]any); ok {
			if _, hasName := def["name"]; hasName {
				def["name"] = productSlug + "-net"
				nets["default"] = def
				doc["networks"] = nets
				networkRenamed = true
			}
		}
	}

	services, _ := doc["services"].(map[string]any)
	if len(services) == 0 {
		return nil, networkRenamed, doc, nil
	}

	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)

	var mappings []portMapping
	next := base
	for _, name := range names {
		svc, _ := services[name].(map[string]any)
		portsRaw, _ := svc["ports"].([]any)
		if len(portsRaw) == 0 {
			continue
		}
		entries := make([]string, 0, len(portsRaw))
		for _, p := range portsRaw {
			if s, ok := p.(string); ok {
				entries = append(entries, s)
			}
		}
		sort.Strings(entries)

		newEntries := make([]string, 0, len(entries))
		changed := false
		for _, entry := range entries {
			containerPort, proto, ok := extractContainerPort(entry)
			if !ok {
				newEntries = append(newEntries, entry)
				continue
			}
			newHost := next
			next++
			mappings = append(mappings, portMapping{Service: name, ContainerPort: containerPort, Proto: proto, NewHostPort: newHost})
			if proto != "" {
				newEntries = append(newEntries, fmt.Sprintf("%d:%s/%s", newHost, containerPort, proto))
			} else {
				newEntries = append(newEntries, fmt.Sprintf("%d:%s", newHost, containerPort))
			}
			changed = true
		}
		if changed {
			svc["ports"] = newEntries
			services[name] = svc
		}
	}
	doc["services"] = services
	return mappings, networkRenamed, doc, nil
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
// whoever fills in health_url during onboarding knows which host port each
// service actually landed on.
func writePortsInfo(repoRoot, productName string, mappings []portMapping) error {
	dir := repoRoot + "/products/" + productName
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	lines := "# Host ports this product was remapped to, by the deploy step's\n" +
		"# port-collision fix. container_port -> host_port, per service.\n"
	for _, m := range mappings {
		lines += fmt.Sprintf("%s: {container_port: %s, host_port: %d}\n", m.Service, m.ContainerPort, m.NewHostPort)
	}
	return os.WriteFile(dir+"/ports.yaml", []byte(lines), 0o644)
}
