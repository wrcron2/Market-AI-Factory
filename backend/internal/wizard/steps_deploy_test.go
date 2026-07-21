package wizard

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wrcron2/market-ai-factory/backend/internal/db"
)

// healthOK is a stand-in product whose health endpoint always returns 200,
// so VerifyHealth passes and the run advances to Publish.
func healthOK(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestDashboardAuthTokenNeverPersistsInRunState is the security regression
// guard: the dashboard auth token is a write-only secret (it lands in
// products.dashboard_auth_token, which the API omits via json:"-"). It must
// NEVER be written into wizard_runs.state, because the run-status endpoint
// serializes that column verbatim back to any client — the same class of
// leak as the QQQ incident (a boundary enforced in the obvious place but
// not in the path that actually carries the value). Secrets ride in Input,
// never in persisted State — the same invariant alpaca_secret already honors.
func TestDashboardAuthTokenNeverPersistsInRunState(t *testing.T) {
	srv := healthOK(t)
	e, d := newTestEngine(t, []Step{VerifyHealth{}, Publish{}})
	id, err := e.StartRun("prod", "https://github.com/x/y", false)
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	const secret = "super-secret-dashboard-token"
	if err := e.Advance(id, map[string]string{
		"health_url":           srv.URL,
		"dashboard_url":        srv.URL,
		"dashboard_auth_token": secret,
	}); err != nil {
		t.Fatalf("advance verify_health: %v", err)
	}

	run, _, _ := d.GetWizardRun(id)
	if contains(string(run.State), secret) {
		t.Fatalf("dashboard_auth_token leaked into persisted run state (the run-status API serves this verbatim): %s", run.State)
	}
}

// TestDashboardAuthTokenReachesProductViaInput proves the fix preserves the
// feature: the token still lands in the product row, it just travels through
// Input at the step that consumes it (Publish) rather than through persisted
// State. Confirms both halves — the product gets the token, and the run state
// never held it.
func TestDashboardAuthTokenReachesProductViaInput(t *testing.T) {
	srv := healthOK(t)
	e, d := newTestEngine(t, []Step{VerifyHealth{}, Publish{}})
	id, err := e.StartRun("prod", "https://github.com/x/y", false)
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	const secret = "tok_live_abc123"
	// verify_health collects only the URLs — no secret.
	if err := e.Advance(id, map[string]string{"health_url": srv.URL, "dashboard_url": srv.URL}); err != nil {
		t.Fatalf("advance verify_health: %v", err)
	}
	// publish collects the secret from Input and writes it straight to the product.
	if err := e.Advance(id, map[string]string{"dashboard_auth_token": secret}); err != nil {
		t.Fatalf("advance publish: %v", err)
	}

	p, err := d.GetProduct("prod")
	if err != nil || p == nil {
		t.Fatalf("get product: %v", err)
	}
	if p.DashboardAuthToken != secret {
		t.Fatalf("token did not reach the product row: got %q", p.DashboardAuthToken)
	}
	run, _, _ := d.GetWizardRun(id)
	if contains(string(run.State), secret) {
		t.Fatalf("token leaked into run state: %s", run.State)
	}
}

// TestPublishPreservesTokenOnBlankReRun guards resumability: runs are
// replayable, so Publish can execute more than once. A re-run that does NOT
// re-supply the token (e.g. a Refresh, or an Advance after a transient block)
// must not blank an already-stored token. Mirrors alpaca's "require the
// secret to change it" behavior, adapted for an optional field.
func TestPublishPreservesTokenOnBlankReRun(t *testing.T) {
	srv := healthOK(t)
	e, d := newTestEngine(t, []Step{VerifyHealth{}, Publish{}})
	id, _ := e.StartRun("prod", "https://github.com/x/y", false)

	const secret = "tok_keepme"
	_ = e.Advance(id, map[string]string{"health_url": srv.URL, "dashboard_url": srv.URL})
	_ = e.Advance(id, map[string]string{"dashboard_auth_token": secret}) // publish with token

	// Re-run Publish.Execute directly with NO token in Input — the stored
	// token must survive rather than being overwritten with "".
	ctx, _ := e.buildCtx(mustRun(t, d, id), map[string]string{})
	if err := (Publish{}).Execute(ctx); err != nil {
		t.Fatalf("re-run publish: %v", err)
	}
	p, _ := d.GetProduct("prod")
	if p.DashboardAuthToken != secret {
		t.Fatalf("blank re-run wiped the stored token: got %q, want %q", p.DashboardAuthToken, secret)
	}
}

// TestPublishPreservesLoginPathOnBlankReRun — the login-path override is a
// documented feature (reachable via the advance API). Like the token, a
// replay of Publish that doesn't re-supply it must preserve the stored value
// rather than silently reverting it to the default, which would break that
// product's auto-login.
func TestPublishPreservesLoginPathOnBlankReRun(t *testing.T) {
	srv := healthOK(t)
	e, d := newTestEngine(t, []Step{VerifyHealth{}, Publish{}})
	id, _ := e.StartRun("prod", "https://github.com/x/y", false)

	_ = e.Advance(id, map[string]string{"health_url": srv.URL, "dashboard_url": srv.URL})
	_ = e.Advance(id, map[string]string{"dashboard_auth_token": "tok", "dashboard_auth_login": "/custom/signin"})

	// Re-run Publish.Execute with NO auth inputs — the custom login path
	// must survive rather than reverting to /api/auth/login.
	ctx, _ := e.buildCtx(mustRun(t, d, id), map[string]string{})
	if err := (Publish{}).Execute(ctx); err != nil {
		t.Fatalf("re-run publish: %v", err)
	}
	p, _ := d.GetProduct("prod")
	if p.DashboardAuthLogin != "/custom/signin" {
		t.Fatalf("blank re-run reverted the custom login path: got %q, want /custom/signin", p.DashboardAuthLogin)
	}
}

func mustRun(t *testing.T, d *db.DB, id int64) *db.WizardRun {
	t.Helper()
	run, _, err := d.GetWizardRun(id)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	return run
}

// portMap builds the port_map shape Deploy.Execute records: service:cport →
// {internal_url, host_port}. host_port is float64 because it round-trips
// through JSON in the real flow.
func portMap(internalURL string, hostPort float64) map[string]any {
	return map[string]any{
		"svc:47331": map[string]any{"internal_url": internalURL, "host_port": hostPort},
	}
}

// TestRewriteToInternalURLAdoptsInternalScheme is the QA regression guard: a
// public URL typed with https:// must be rewritten to the internal Docker
// URL's scheme (http), not left as https — the Docker network doesn't speak
// TLS, so a preserved https would make every later health probe and proxy
// hop fail a TLS handshake against a plaintext container.
func TestRewriteToInternalURLAdoptsInternalScheme(t *testing.T) {
	pm := portMap("http://openalice:47331", 10100)
	got := rewriteToInternalURL("https://129.159.146.157:10100/dash", pm)
	if got != "http://openalice:47331/dash" {
		t.Fatalf("scheme not adopted from internal_url: got %q, want http://openalice:47331/dash", got)
	}
}

// TestRewriteToInternalURLMatchingPort characterizes the http→http happy path.
func TestRewriteToInternalURLMatchingPort(t *testing.T) {
	pm := portMap("http://openalice:47331", 10100)
	got := rewriteToInternalURL("http://129.159.146.157:10100/login?x=1", pm)
	if got != "http://openalice:47331/login?x=1" {
		t.Fatalf("host/path/query not preserved: got %q", got)
	}
}

// TestRewriteToInternalURLNoMatchPassthrough — a URL whose port matches no
// port_map entry (e.g. the operator typed the internal URL directly) is left
// untouched.
func TestRewriteToInternalURLNoMatchPassthrough(t *testing.T) {
	pm := portMap("http://openalice:47331", 10100)
	in := "http://openalice:47331/already-internal"
	if got := rewriteToInternalURL(in, pm); got != in {
		t.Fatalf("non-matching port should pass through unchanged: got %q", got)
	}
}
