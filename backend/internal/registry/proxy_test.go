package registry

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/wrcron2/market-ai-factory/backend/internal/db"
)

// newTestHandler wires a Handler against a fresh in-memory DB, mirroring
// the pattern in wizard/engine_test.go. Returns the handler and the DB
// so each test can seed products directly.
func newTestHandler(t *testing.T) (*Handler, *db.DB) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return New(database, zap.NewNop(), nil, t.TempDir()), database
}

func mustInsert(t *testing.T, d *db.DB, p *db.Product) {
	t.Helper()
	if p.Status == "" {
		p.Status = db.StatusLive
	}
	if _, err := d.InsertProduct(p); err != nil {
		t.Fatalf("insert %s: %v", p.Name, err)
	}
}

// upstreamOK starts a server that returns the canned body; all upstream
// paths reach it (the proxy is per-product, so routing inside the
// upstream is the product's own concern).
func upstreamOK(t *testing.T, body, contentType string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = io.WriteString(w, body)
	}))
}

// --- targetFor ---------------------------------------------------------------

func TestTargetForPrefersDashboardURL(t *testing.T) {
	p := &db.Product{Name: "x", DashboardURL: "http://up:80/", HealthURL: "http://health:90/"}
	u, err := targetFor(p, "sub")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if u.Host != "up:80" {
		t.Fatalf("expected dashboard_url host, got %q", u.Host)
	}
	if u.Path != "" {
		t.Fatalf("expected path stripped so ReverseProxy can re-set it, got %q", u.Path)
	}
}

func TestTargetForFallsBackToHealthURL(t *testing.T) {
	p := &db.Product{Name: "x", DashboardURL: "", HealthURL: "http://openalice:47331/"}
	u, err := targetFor(p, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if u.Host != "openalice:47331" {
		t.Fatalf("expected internal container host, got %q", u.Host)
	}
}

func TestTargetForRejectsMissingURLs(t *testing.T) {
	if _, err := targetFor(&db.Product{}, ""); err == nil {
		t.Fatal("expected error when neither URL set")
	}
}

func TestTargetForRejectsInvalidURL(t *testing.T) {
	if _, err := targetFor(&db.Product{DashboardURL: "://no-scheme"}, ""); err == nil {
		t.Fatal("expected error for schemeless URL")
	}
}

// R6 fix: a non-http(s) scheme in the DB must be rejected so a future
// bug or manual edit can't coax the proxy into a file:// or gopher://
// request via the default Transport.
func TestTargetForRejectsNonHTTPScheme(t *testing.T) {
	for _, scheme := range []string{"file", "gopher", "ftp", "data"} {
		p := &db.Product{DashboardURL: scheme + "://localhost/x"}
		if _, err := targetFor(p, ""); err == nil {
			t.Fatalf("expected error for %s:// scheme", scheme)
		}
	}
}

// --- splitProxyPath ----------------------------------------------------------

func TestSplitProxyPath(t *testing.T) {
	cases := []struct {
		in           string
		wantName     string
		wantSub      string
		wantOK       bool
	}{{
		in: "open-alice/proxy/", wantName: "open-alice", wantSub: "", wantOK: true,
	}, {
		in: "open-alice/proxy/login", wantName: "open-alice", wantSub: "login", wantOK: true,
	}, {
		in: "open-alice/proxy/a/b/c", wantName: "open-alice", wantSub: "a/b/c", wantOK: true,
	}, {
		// bare proxy with no trailing slash — Go's ServeMux would have
		// redirected, but be explicit anyway.
		in: "open-alice/proxy", wantName: "open-alice", wantSub: "", wantOK: true,
	}, {
		in: "open-alice/pause", wantOK: false, // not a proxy route, Product handler's other branch handles it
	}, {
		in: "open-alice", wantOK: false,
	}, {
		in: "/proxy/", wantOK: false, // missing name
	}}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			gotName, gotSub, gotOK := splitProxyPath(c.in)
			if gotOK != c.wantOK {
				t.Fatalf("ok: want %v got %v (name=%q sub=%q)", c.wantOK, gotOK, gotName, gotSub)
			}
			if !c.wantOK {
				return
			}
			if gotName != c.wantName {
				t.Errorf("name: want %q got %q", c.wantName, gotName)
			}
			if gotSub != c.wantSub {
				t.Errorf("sub: want %q got %q", c.wantSub, gotSub)
			}
		})
	}
}

// --- rewriteUpstreamURL ------------------------------------------------------

func TestRewriteUpstreamURL(t *testing.T) {
	up, _ := url.Parse("http://openalice:47331")
	const product = "open-alice"

	cases := []struct {
		name      string
		in        string
		want      string
		wantChg  bool
	}{{
		name: "absolute upstream url → proxy path",
		in:   "http://openalice:47331/login",
		want: "/api/products/open-alice/proxy/login", wantChg: true,
	}, {
		name: "absolute upstream url with query",
		in:   "http://openalice:47331/api/foo?bar=1",
		want: "/api/products/open-alice/proxy/api/foo?bar=1", wantChg: true,
	}, {
		name: "absolute upstream url with fragment",
		in:   "http://openalice:47331/x#y",
		want: "/api/products/open-alice/proxy/x#y", wantChg: true,
	}, {
		name: "different host → left alone",
		in:   "https://accounts.google.com/o/oauth2/auth",
		want: "https://accounts.google.com/o/oauth2/auth", wantChg: false,
	}, {
		name: "relative path → unchanged (browser resolves)",
		in:   "/login",
		want: "/login", wantChg: false, // rewriteUpstreamURL returns it as written; the iframe does the right thing
	}, {
		name: "empty input",
		in:   "", want: "", wantChg: false,
	}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, chg := rewriteUpstreamURL(c.in, up, product)
			if got != c.want || chg != c.wantChg {
				t.Fatalf("got (%q,%v) want (%q,%v)", got, chg, c.want, c.wantChg)
			}
		})
	}
}

// --- stripCookieDomain -------------------------------------------------------

func TestStripCookieDomain(t *testing.T) {
	cases := []struct {
		in, want string
	}{{
		in:   "session=abc; Domain=openalice; Path=/",
		want: "session=abc; Path=/",
	}, {
		in:   "token=xyz; Domain=openalice",
		want: "token=xyz",
	}, {
		in:   "session=abc; Domain=openalice; Path=/; HttpOnly",
		want: "session=abc; Path=/; HttpOnly",
	}, {
		in:   "session=abc; Path=/; HttpOnly",
		want: "session=abc; Path=/; HttpOnly", // no Domain → unchanged textually
	}, {
		in:   "session=abc; domain=openalice", // case-insensitive match
		want: "session=abc",
	}}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := stripCookieDomain(c.in)
			if got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

// --- end-to-end ServeProxy ---------------------------------------------------

// TestServeProxyRequiresLiveProduct confirms the central security
// boundary: only LIVE products are reachable through the proxy. A
// PAUSED or DRAFT product's dashboard must 404/503 — we never expose a
// stopped stack's containers, even if the container is somehow still
// running and reachable internally.
func TestServeProxyRequiresLiveProduct(t *testing.T) {
	up := upstreamOK(t, "should not reach", "text/plain")
	t.Cleanup(up.Close)

	for _, status := range []string{db.StatusPaused, db.StatusDraft, db.StatusError} {
		t.Run(status, func(t *testing.T) {
			h, d := newTestHandler(t)
			mustInsert(t, d, &db.Product{Name: "p", Status: status, DashboardURL: up.URL})
			req := httptest.NewRequest("GET", "/api/products/p/proxy/", nil)
			rec := httptest.NewRecorder()
			h.ServeProxy(rec, req)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("want 503 for %s, got %d (body=%q)", status, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestServeProxy404OnUnknownProduct — an unknown product can never be
// reached through the proxy, regardless of what path is attempted.
// Covers the SSRF case where an attacker would try /api/products/localhost/proxy/
// to bounce through the Factory to the host's loopback.
func TestServeProxy404OnUnknownProduct(t *testing.T) {
	h, _ := newTestHandler(t)
	req := httptest.NewRequest("GET", "/api/products/localhost/proxy/", nil)
	rec := httptest.NewRecorder()
	h.ServeProxy(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 for unknown product, got %d", rec.Code)
	}
}

// TestServeProxyReachesUpstream is the happy path — a LIVE product
// proxies to its dashboard URL and returns the upstream's body
// verbatim. Confirms wiring end-to-end through the real reverse proxy.
func TestServeProxyReachesUpstream(t *testing.T) {
	up := upstreamOK(t, "DASHBOARD_BODY", "text/html")
	t.Cleanup(up.Close)

	h, d := newTestHandler(t)
	mustInsert(t, d, &db.Product{Name: "p", Status: db.StatusLive, DashboardURL: up.URL})

	req := httptest.NewRequest("GET", "/api/products/p/proxy/", nil)
	rec := httptest.NewRecorder()
	h.ServeProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "DASHBOARD_BODY" {
		t.Fatalf("body: want %q got %q", "DASHBOARD_BODY", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type lost: %q", ct)
	}
}

// TestServeProxyRewritesUpstreamRedirects confirms a Location: header
// pointing at the upstream's own hostname is rewritten to the proxy's
// URL — otherwise the browser would follow the unreachable internal
// hostname and 502.
func TestServeProxyRewritesUpstreamRedirects(t *testing.T) {
	// Use a writable upstream that returns a Location to itself; the
	// proxy must rewrite it to the proxy route. We can't bind
	// "openalice.test:47331", but rewriteUpstreamURL is host-agnostic
	// (matched against the upstream's own Host), so any httptest
	// upstream behaves the same as an internal one.
	var up *httptest.Server
	up = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, up.URL+"/login", http.StatusSeeOther)
	}))
	t.Cleanup(up.Close)

	h, d := newTestHandler(t)
	mustInsert(t, d, &db.Product{Name: "p", Status: db.StatusLive, DashboardURL: up.URL})

	req := httptest.NewRequest("GET", "/api/products/p/proxy/", nil)
	rec := httptest.NewRecorder()
	h.ServeProxy(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want 303 passthrough, got %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	want := "/api/products/p/proxy/login"
	if loc != want {
		t.Fatalf("Location: want %q got %q", want, loc)
	}
}

// TestServeProxyStripsUpstreamSetCookieDomain — a Set-Cookie pinned to
// the upstream's hostname wouldn't be accepted by the browser at the
// Factory's host; strip Domain so it defaults to ours.
func TestServeProxyStripsUpstreamSetCookieDomain(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// First cookie with Domain attribute — should be stripped.
		http.SetCookie(w, &http.Cookie{Name: "sess", Value: "abc", Domain: "openalice.test"})
		// Second cookie WITHOUT Domain — must survive untouched (mixed
		// cookie responses are normal; the rewrite mustn't break them).
		w.Header().Add("Set-Cookie", "theme=dark; Path=/")
	}))
	t.Cleanup(up.Close)

	h, d := newTestHandler(t)
	mustInsert(t, d, &db.Product{Name: "p", Status: db.StatusLive, DashboardURL: up.URL})

	req := httptest.NewRequest("GET", "/api/products/p/proxy/", nil)
	rec := httptest.NewRecorder()
	h.ServeProxy(rec, req)

	cookies := rec.Result().Header.Values("Set-Cookie")
	if len(cookies) != 2 {
		t.Fatalf("want 2 cookies, got %d: %v", len(cookies), cookies)
	}
	for _, c := range cookies {
		if strings.Contains(strings.ToLower(c), "domain=") {
			t.Fatalf("Domain not stripped: %q", c)
		}
	}
}

// TestServeProxyPathTraversalCollapsesDDot confirms ".." is cleaned
// before forwarding — a request like
// /api/products/p/proxy/../../etc/passwd is forwarded as
// /etc/passwd to the upstream only (which is fine — the upstream is an
// app server, not a fileserver), AND our suffix trailing-slash
// preservation doesn't accidentally keep ".." in the path.
func TestServeProxyPathTraversalCollapsesDDot(t *testing.T) {
	var seenPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up.Close)

	h, d := newTestHandler(t)
	mustInsert(t, d, &db.Product{Name: "p", Status: db.StatusLive, DashboardURL: up.URL})

	req := httptest.NewRequest("GET", "/api/products/p/proxy/../../etc/passwd", nil)
	rec := httptest.NewRecorder()
	h.ServeProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	// The path that hits the upstream must be cleaned — no "..".
	if strings.Contains(seenPath, "..") {
		t.Fatalf("upstream saw a path with .. : %q", seenPath)
	}
	if seenPath != "/etc/passwd" {
		t.Fatalf("upstream path: want /etc/passwd, got %q", seenPath)
	}
}

// TestServeProxyStripsHostHeader confirms the upstream doesn't see the
// Factory's inbound Host header (or the human's intended-host name) —
// the proxy sets Host to the upstream's, so apps that tie cookies/auth
// to the Host get the right one.
func TestServeProxyStripsHostHeader(t *testing.T) {
	var seenHost string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHost = r.Host
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up.Close)

	upURL, _ := url.Parse(up.URL)
	h, d := newTestHandler(t)
	mustInsert(t, d, &db.Product{Name: "p", Status: db.StatusLive, DashboardURL: up.URL})

	req := httptest.NewRequest("GET", "/api/products/p/proxy/", nil)
	req.Host = "factory.example.com:9080"
	rec := httptest.NewRecorder()
	h.ServeProxy(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if seenHost != upURL.Host {
		t.Fatalf("upstream saw Host %q, want %q (the upstream's own host)", seenHost, upURL.Host)
	}
}

// TestServeProxyPreservesTrailingSlashOnRedirectBound confirms a
// request to /api/products/p/proxy/x/ keeps the trailing slash, so an
// upstream that 301s /x → /x/ doesn't ping-pong through the proxy.
func TestServeProxyPreservesTrailingSlash(t *testing.T) {
	var seenPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up.Close)

	h, d := newTestHandler(t)
	mustInsert(t, d, &db.Product{Name: "p", Status: db.StatusLive, DashboardURL: up.URL})

	req := httptest.NewRequest("GET", "/api/products/p/proxy/foo/", nil)
	rec := httptest.NewRecorder()
	h.ServeProxy(rec, req)

	if !strings.HasSuffix(seenPath, "/") {
		t.Fatalf("trailing slash dropped: %q", seenPath)
	}
}

// TestServeProxyReturns503OnUnknownProduct confirms we don't leak the
// upstream hostname from internal errors — the response body is a
// generic string, not a container name.
func TestServeProxyDoesNotLeakUpstreamHostname(t *testing.T) {
	// Use a port that's almost certainly not listening — the connection
	// will fail, the ErrorHandler should fire, and its body must not
	// contain "openalice" or "47331".
	const internalURL = "http://openalice:47331/"
	h, d := newTestHandler(t)
	mustInsert(t, d, &db.Product{Name: "p", Status: db.StatusLive, DashboardURL: internalURL})

	req := httptest.NewRequest("GET", "/api/products/p/proxy/", nil)
	rec := httptest.NewRecorder()
	h.ServeProxy(rec, req)

	if rec.Code != http.StatusBadGateway && rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("want 502/504 (upstream unreachable), got %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "openalice") || strings.Contains(body, "47331") {
		t.Fatalf("upstream hostname leaked in error body: %q", body)
	}
}

// R3 fix: Forwarded, X-Real-IP, X-Forwarded-Server, X-Forwarded-Port
// must be stripped from the request before it reaches the upstream —
// the product app has no reason to learn the human's browser IP or the
// Factory's internal topology via these forwarding hints.
func TestServeProxyStripsForwardedHeaders(t *testing.T) {
	var seenHeaders http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up.Close)

	h, d := newTestHandler(t)
	mustInsert(t, d, &db.Product{Name: "p", Status: db.StatusLive, DashboardURL: up.URL})

	req := httptest.NewRequest("GET", "/api/products/p/proxy/", nil)
	// Simulate a browser request that picked up forwarding headers on
	// its way through the Factory's nginx → backend chain.
	req.Header.Set("X-Forwarded-For", "203.0.113.42")
	req.Header.Set("X-Forwarded-Host", "factory.example.com:9000")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Server", "nginx-factory")
	req.Header.Set("X-Forwarded-Port", "9000")
	req.Header.Set("X-Real-IP", "203.0.113.42")
	req.Header.Set("Forwarded", `for="203.0.113.42";host="factory.example.com"`)

	rec := httptest.NewRecorder()
	h.ServeProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	for _, h := range []string{
		"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto",
		"X-Forwarded-Server", "X-Forwarded-Port", "X-Real-IP", "Forwarded",
	} {
		if seenHeaders.Get(h) != "" {
			t.Fatalf("header %q was not stripped, upstream saw %q", h, seenHeaders.Get(h))
		}
	}
}

// R5 fix: the upstream's identifying response headers (Server,
// X-Powered-By, Via) must be stripped so the browser can't fingerprint
// the product's tech stack from the proxied responses.
func TestServeProxyStripsUpstreamIdentifyingHeaders(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "openalice/1.0")
		w.Header().Set("X-Powered-By", "Node.js/20")
		w.Header().Set("Via", "1.1 internal-proxy")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up.Close)

	h, d := newTestHandler(t)
	mustInsert(t, d, &db.Product{Name: "p", Status: db.StatusLive, DashboardURL: up.URL})

	req := httptest.NewRequest("GET", "/api/products/p/proxy/", nil)
	rec := httptest.NewRecorder()
	h.ServeProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	for _, h := range []string{"Server", "X-Powered-By", "Via"} {
		if v := rec.Result().Header.Get(h); v != "" {
			t.Fatalf("response header %q was not stripped, got %q", h, v)
		}
	}
}

// R1 fix: WebSocket upgrade requests must not inherit the 30s
// proxyTimeout — that would kill live-update streams at 30s. We can't
// easily wait 30s in a unit test, but we can verify the upgrade request
// reaches the upstream with its headers intact (the code path that
// branches on isUpgrade), and that the upstream sees the Upgrade header.
func TestServeProxyPassesWebSocketUpgradeHeaders(t *testing.T) {
	var seenUpgrade, seenConnection string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenUpgrade = r.Header.Get("Upgrade")
		seenConnection = r.Header.Get("Connection")
		w.WriteHeader(http.StatusOK) // not a real WS handshake, just verify headers
	}))
	t.Cleanup(up.Close)

	h, d := newTestHandler(t)
	mustInsert(t, d, &db.Product{Name: "p", Status: db.StatusLive, DashboardURL: up.URL})

	req := httptest.NewRequest("GET", "/api/products/p/proxy/", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")

	rec := httptest.NewRecorder()
	h.ServeProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if seenUpgrade != "websocket" {
		t.Fatalf("upstream saw Upgrade %q, want %q", seenUpgrade, "websocket")
	}
	if seenConnection != "Upgrade" {
		t.Fatalf("upstream saw Connection %q, want %q", seenConnection, "Upgrade")
	}
}
