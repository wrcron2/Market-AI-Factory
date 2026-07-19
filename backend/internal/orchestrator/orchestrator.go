// Package orchestrator shells out to git and docker-compose to manage
// per-product stacks. Kept deliberately thin: every command's combined
// output is returned so wizard steps can surface real errors, never
// swallow them.
package orchestrator

import (
	"fmt"
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

// ComposeUp builds and starts the stack defined in dir.
func ComposeUp(dir, composeFile string) (string, error) {
	return run(15*time.Minute, "docker-compose", "-f", dir+"/"+composeFile, "up", "-d", "--build")
}

// ComposeDown stops a product stack (kill switch / PAUSE).
func ComposeDown(dir, composeFile string) (string, error) {
	return run(5*time.Minute, "docker-compose", "-f", dir+"/"+composeFile, "stop")
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
