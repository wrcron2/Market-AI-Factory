// Package orchestrator shells out to git and docker-compose to manage
// per-product stacks. Kept deliberately thin: every command's combined
// output is returned so wizard steps can surface real errors, never
// swallow them.
package orchestrator

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// LsRemoteHead returns the HEAD commit SHA of a remote repo (also proves
// the repo exists and is reachable without cloning it).
func LsRemoteHead(repoURL string) (string, error) {
	out, err := run(30*time.Second, "git", "ls-remote", repoURL, "HEAD")
	if err != nil {
		return "", err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", fmt.Errorf("no HEAD ref returned for %s", repoURL)
	}
	return fields[0], nil
}

// CloneAt clones repoURL into dir and checks out the pinned SHA.
func CloneAt(repoURL, sha, dir string) error {
	if _, err := run(5*time.Minute, "git", "clone", "--depth", "50", repoURL, dir); err != nil {
		return err
	}
	if _, err := run(1*time.Minute, "git", "-C", dir, "checkout", sha); err != nil {
		return err
	}
	return nil
}

// ComposeUp builds and starts the stack defined by the given compose file(s) in dir.
func ComposeUp(dir string, composeFiles ...string) (string, error) {
	return run(15*time.Minute, "docker-compose", composeArgs(dir, composeFiles, "up", "-d", "--build")...)
}

// ComposeDown stops a product stack (kill switch / PAUSE). Must be given the
// same compose file set as the matching ComposeUp so compose resolves the
// same project/services.
func ComposeDown(dir string, composeFiles ...string) (string, error) {
	return run(5*time.Minute, "docker-compose", composeArgs(dir, composeFiles, "stop")...)
}

// ComposeFiles returns the compose file to use for a product's stack: the
// factory-generated, port-remapped file if the deploy step created one (the
// repo published fixed host ports that needed reassigning to avoid a
// collision with another product), else the product's own docker-compose.yml,
// completely unmodified. Never both — compose concatenates `ports:` lists
// across -f files rather than replacing them, so layering would leave the
// original colliding binding in place alongside the new one.
func ComposeFiles(dir string) []string {
	if _, err := os.Stat(dir + "/docker-compose.factory.yml"); err == nil {
		return []string{"docker-compose.factory.yml"}
	}
	return []string{"docker-compose.yml"}
}

func composeArgs(dir string, composeFiles []string, cmd ...string) []string {
	args := make([]string, 0, len(composeFiles)*2+len(cmd))
	for _, f := range composeFiles {
		args = append(args, "-f", dir+"/"+f)
	}
	return append(args, cmd...)
}

func run(timeout time.Duration, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return "", fmt.Errorf("%s timed out after %s", name, timeout)
	}
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %v — %s", name, strings.Join(args, " "), err, truncate(string(out), 500))
	}
	return string(out), nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
