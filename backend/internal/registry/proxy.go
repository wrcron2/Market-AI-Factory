package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/wrcron2/market-ai-factory/backend/internal/db"
)

// ProductProxy exposes a product's own dashboard through the Factory's
// listening port, so a human can reach it at
// `/api/products/<name>/proxy/<whatever>` — no published host port on the
// product, no per-product OCI Security List rule required. This is the
// permanent fix for the design flaw that bit open-alice: dashboards going
// out over the public internet (which the cloud layer blocks) when they
// never needed to — they only need to be reachable from inside the box,
// where Docker DNS already makes them so.
//
// The Factory's own frontend (port 9000) is what the human points a
// browser at; the dashboard route mounts this proxy in an iframe. The
// backend container is on factory-net (its own compose stack), so it can
// reach any product's container by name. For adopted products that
// predate the Factory (their dashboard_url is a public-IP that already
// works), the upstream is exactly that URL — the proxy just routes
// through it consistently so the frontend never has to know the
// difference.

// proxyTimeout bounds upstream calls — a product's dashboard shouldn't
// take 30s to respond, and if it does something is wrong worth surfacing
// rather than hanging the human's browser.
const proxyTimeout = 30 * time.Second

// ServeProxy handles /api/products/{name}/proxy/* (and the bare
// /api/products/{name}/proxy form). Method-agnostic — ReverseProxy
// preserves GET/POST/etc., and Upgrade: websocket upgrades ride through
// too (httputil.ReverseProxy has supported that since Go 1.12), which
// matters because some SPA dashboards (OpenAlice's included) use WS for
// live updates.
func (h *Handler) ServeProxy(w http.ResponseWriter, r *http.Request) {
	// Path shape after the mux strips "/api/products/":
	//   <name>/proxy             -> upstream "/"
	//   <name>/proxy/<rest...>   -> upstream "/<rest>"
	rest := strings.TrimPrefix(r.URL.Path, "/api/products/")
	name, subPath, ok := splitProxyPath(rest)
	if !ok {
		http.NotFound(w, r)
		return
	}
	p, err := h.db.GetProduct(name)
	if err != nil || p == nil {
		http.Error(w, "product not found", http.StatusNotFound)
		return
	}
	// LIVE-only — proxying a PAUSED/ERROR product's dashboard would
	// likely 502 anyway (its stack is down), but more importantly this
	// is a security boundary: nothing about a non-LIVE product should
	// be reachable through the Factory. The registry status is the
	// kill-switch here too.
	if p.Status != db.StatusLive {
		http.Error(w, "product is "+strings.ToLower(p.Status)+" — start it first", http.StatusServiceUnavailable)
		return
	}

	upstream, err := targetFor(p, subPath)
	if err != nil {
		h.logger.Warn("proxy.target_resolve_failed",
			zap.String("product", name), zap.Error(err))
		http.Error(w, "product has no reachable dashboard URL", http.StatusBadGateway)
		return
	}

	// RewriteProxyDirector redirects the incoming request at upstream,
	// but we keep its path-cleaning behavior strict: a ".." in the subPath
	// must never escape — if the requested subPath is "/../etc/passwd"
	// we serve "/etc/passwd" only inside upstream's view (which is
	// irrelevant, since the upstream is itself a single app, not a
	// fileserver of the host's filesystem). path.Clean collapses the
	// ".." so we forward exactly what was asked, semantically.
	//
	// Then we restore a trailing slash if the original asked for one —
	// path.Clean's documented behavior is to strip a trailing slash
	// from non-root paths, which would 301-redirect ping-pong through
	// the proxy forever if the upstream enforces it.
	hadTrailingSlash := strings.HasSuffix(subPath, "/")
	r.URL.Path = path.Clean("/" + subPath)
	if hadTrailingSlash && !strings.HasSuffix(r.URL.Path, "/") {
		r.URL.Path += "/"
	}
	if r.URL.RawPath != "" {
		// Preserve RawPath (escaped form) so a query-less %2F in the
		// path survives intact; Go's ReverseProxy uses RawPath when
		// set, falling back to Path's escaped form otherwise.
		r.URL.RawPath = path.Clean("/" + r.URL.RawPath)
	}

	// WebSocket upgrades (and other long-lived Upgrade: requests) must
	// NOT inherit the 30s proxyTimeout — that would tear down the
	// hijacked socket at 30s, killing live-update streams the code
	// claims to support. Use a generous deadline for upgrade requests
	// (the connection is hijacked after the handshake, so the deadline
	// only bounds the initial upgrade roundtrip, not the session).
	isUpgrade := strings.EqualFold(r.Header.Get("Connection"), "upgrade") ||
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
	if isUpgrade {
		ctx, cancel := context.WithTimeout(r.Context(), 24*time.Hour)
		defer cancel()
		r = r.WithContext(ctx)
	} else {
		ctx, cancel := context.WithTimeout(r.Context(), proxyTimeout)
		defer cancel()
		r = r.WithContext(ctx)
	}

	proxy := httputil.NewSingleHostReverseProxy(upstream)
	// Resolve the product's upstream session cookie (best-effort auto-
	// login). OpenAlice and similar products gate the dashboard behind
	// a token-exchange login; the proxy caches the session per product
	// and injects it into the request's Cookie header so the human
	// never sees the product's own login wall. No-op when the product
	// has no DashboardAuthToken.
	sessionCookie := h.sessions.cookieFor(name, p, upstream)
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		// Drop the inbound Host header — single-host reverse proxy sets
		// it to upstream.Host already, but make it explicit so a future
		// change to proxy behavior can't leak the Factory's hostname to
		// the product. A product app that ties auth cookies to a
		// specific Host would otherwise see "129.159.146.157:9080" and
		// reject or set cookies for the wrong origin.
		req.Host = upstream.Host
		// Inject the upstream session cookie (best-effort: empty when
		// the product has no auth token OR when login is in backoff).
		// Merge with any browser-sent cookies the human may have
		// already for this proxy host — but in practice the browser
		// won't have an upstream-named cookie (Set-Cookie's Domain was
		// stripped upstream so it was set for the Factory's host, not
		// the upstream), so we just set rather than append.
		if sessionCookie != "" {
			// The Factory's cached session cookie is authoritative. Merge
			// it over any cookies the browser sent, dropping any stale copy
			// of the SAME name: because the base proxy strips Set-Cookie
			// Domain, the browser stores the upstream's session cookie under
			// the Factory's host and sends it back here — a value that can
			// go stale (server-side expiry) while the browser keeps it. If
			// that stale copy rode alongside ours, a first-match upstream
			// (Koa's cookie parser) would keep using it and the cache's
			// re-login could never take hold: a permanent 401 loop. Other
			// browser cookies (theme, etc.) are preserved.
			req.Header.Set("Cookie", mergeSessionCookie(req.Header.Get("Cookie"), sessionCookie))
		}
		// X-Forwarded-* and friends are deleted in the Transport below,
		// NOT here — ReverseProxy.ServeHTTP re-adds X-Forwarded-For
		// from RemoteAddr AFTER the Director runs, so a Director-side
		// delete would be silently overridden. The Transport is the
		// last stop before the wire; stripping there actually sticks.
	}
	// Custom Transport: strip every forwarding header right before the
	// request hits the wire. ReverseProxy.ServeHTTP adds X-Forwarded-For
	// after the Director, so Director-side deletion is futile — the
	// Transport is the only layer that can reliably remove these. The
	// product app has no reason to learn the human's browser IP, the
	// Factory's hostname, or the upstream's scheme via these hints.
	proxy.Transport = &forwardingHeaderStripper{base: http.DefaultTransport}
	proxy.ModifyResponse = func(resp *http.Response) error {
		// Session-expiry signal: if upstream returns 401/403 while we
		// had a session cookie in flight, the cached cookie is stale
		// (or the product rotated it). Invalidate the cache entry so
		// the next request re-logs in. We don't retry here — the
		// human's browser would still see the 401 page, but a Reload
		// (or just navigating to another dashboard route) triggers a
		// fresh login. This keeps the cache self-healing without
		// adding latency on the warm path.
		if sessionCookie != "" && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			h.sessions.invalidate(name)
		}
		// Rewrite Location: headers on 3xx so a redirect to the
		// upstream's own URL stays routed back through the proxy.
		// e.g. upstream returns "Location: http://openalice:47331/login"
		// — the browser can't reach that; rewrite it to
		// "/api/products/open-alice/proxy/login".
		loc := resp.Header.Get("Location")
		if loc != "" {
			rewritten, changed := rewriteUpstreamURL(loc, upstream, name)
			if changed {
				resp.Header.Set("Location", rewritten)
			}
		}
		// Same for Link: <...>; rel=... — rare but HATEOAS APIs use
		// it, and we don't want a SPA's preload to start fetching
		// unreachable upstream URLs. Note: proper RFC 8288 Link syntax
		// wraps the URL in <...>, so a bare url.Parse treats it as a
		// relative path and leaves it alone — only bare absolute URLs
		// pointing at the upstream get rewritten here. Real HATEOAS
		// Link headers with <url> syntax are NOT rewritten by this
		// path; that's a known limitation, acceptable for a dashboard
		// proxy (HATEOAS APIs almost never serve HTML dashboards).
		if links := resp.Header.Values("Link"); len(links) > 0 {
			for i, l := range links {
				rewritten, changed := rewriteUpstreamURL(l, upstream, name)
				if changed {
					links[i] = rewritten
				}
			}
			resp.Header.Del("Link")
			for _, l := range links {
				resp.Header.Add("Link", l)
			}
		}
		// Cookies: a Set-Cookie Domain attribute pinned to the
		// upstream's hostname won't be accepted by the browser (wrong
		// origin), so the cookie is lost entirely. Strip Domain so
		// each cookie defaults to the Factory's host — which the
		// browser will then send back to the proxy. Multiple Set-Cookie
		// headers per response are common (one cookie each), and the
		// cookie at each index may or may not have a Domain= attr — so
		// rewrite in place per header value rather than via Set (which
		// would collapse all of them into one).
		cookies := resp.Header.Values("Set-Cookie")
		if len(cookies) > 0 {
			anyChanged := false
			for i, c := range cookies {
				if strings.Contains(strings.ToLower(c), "domain=") {
					cookies[i] = stripCookieDomain(c)
					anyChanged = true
				}
			}
			if anyChanged {
				resp.Header.Del("Set-Cookie")
				for _, c := range cookies {
					resp.Header.Add("Set-Cookie", c)
				}
			}
		}
		// Content-Security-Policy frame-ancestors / X-Frame-Options
		// would block the iframe in the frontend route. Drop them —
		// we're letting the Factory's own CSP govern framing instead.
		resp.Header.Del("X-Frame-Options")
		resp.Header.Del("Content-Security-Policy")
		resp.Header.Del("Content-Security-Policy-Report-Only")
		// Strip identifying headers from the upstream's response so the
		// browser can't fingerprint the product's tech stack from the
		// proxy responses (defense in depth, not a security boundary).
		resp.Header.Del("Server")
		resp.Header.Del("X-Powered-By")
		resp.Header.Del("Via")
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, req *http.Request, err error) {
		h.logger.Warn("proxy.upstream_failed",
			zap.String("product", name),
			zap.String("upstream", upstream.String()),
			zap.Error(err))
		// Don't leak the upstream's internal hostname in the response
		// body — it's useful in logs (above) but not for a browser user.
		if errors.Is(err, context.DeadlineExceeded) {
			http.Error(w, "product dashboard timed out", http.StatusGatewayTimeout)
			return
		}
		http.Error(w, "product dashboard unreachable", http.StatusBadGateway)
	}
	proxy.FlushInterval = -1 // stream SSE/chunked responses through immediately
	proxy.ServeHTTP(w, r)
}

// targetFor picks the upstream URL the proxy will hit. For a non-adopted
// product, the wizard recorded health_url as the internal container URL
// (e.g. "http://openalice:47331/") — that's the dashboard's host:port and
// it's reachable from the Factory's container via Docker DNS. For an
// adopted product, dashboard_url (NOT health_url — that's an API health
// probe, not the SPA root) is the upstream, e.g.
// "http://129.159.146.157:3000" for Market-AI's own deploy.
//
// schema/spec sentinel: dashboard_url takes precedence when it's an
// http(s) URL, falling back to health_url. For products still in the
// wizard (no dashboard URL recorded), health_url is a last resort so a
// half-onboarded product won't 502 mysteriously.
func targetFor(p *db.Product, subPath string) (*url.URL, error) {
	raw := p.DashboardURL
	if raw == "" {
		raw = p.HealthURL
	}
	if raw == "" {
		return nil, errors.New("product has neither dashboard_url nor health_url")
	}
	// Strip any trailing slash on the base so we always join cleanly
	// with the subPath's leading slash.
	raw = strings.TrimRight(raw, "/")
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("invalid upstream URL: " + raw)
	}
	// Defense in depth: even though the wizard controls what lands in
	// dashboard_url/health_url, reject any non-http(s) scheme so a
	// future bug or manual DB edit can't coax the proxy into a file://
	// or gopher:// request via the default Transport.
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("upstream URL must be http or https, got " + parsed.Scheme)
	}
	parsed.Path = "" // SingleHostReverseProxy sets it from the request
	parsed.RawPath = ""
	return parsed, nil
}

// splitProxyPath takes the part of the URL after "/api/products/" and
// returns (productName, subPath, true) if it's a /proxy/ route, else
// (..., false). The subPath is everything after "proxy/" (or "" if
// bare), preserving the structure the proxy will forward.
//
// "/open-alice/proxy/"           -> ("open-alice", "",             true)
// "/open-alice/proxy/login"      -> ("open-alice", "login",        true)
// "/open-alice/proxy/a/b?c=1"   -> ("open-alice", "a/b",          true)
//
//	(the query string is on r.URL, not in rest)
//
// "/market-ai/pause"             -> not a proxy route; (..., false)
func splitProxyPath(rest string) (name, subPath string, ok bool) {
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 {
		return "", "", false
	}
	if parts[1] != "proxy" {
		return "", "", false
	}
	name = parts[0]
	if name == "" {
		return "", "", false
	}
	if len(parts) == 3 {
		subPath = parts[2]
	}
	return name, subPath, true
}

// rewriteUpstreamURL converts absolute upstream URLs (the upstream's own
// hostname embedded in a Location: or Link: response header) into a path
// under the proxy route the browser can actually reach. Relative URLs
// ("/login") already work via the iframe's own navigation — the iframe's
// document URL is already under /api/products/<name>/proxy/..., so a
// relative `/login` resolves to /api/products/<name>/proxy/login
// automatically. Rewriting those too would double-prefix and break, so we
// only touch absolute upstream URLs.
func rewriteUpstreamURL(raw string, upstream *url.URL, productName string) (string, bool) {
	if raw == "" {
		return raw, false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw, false
	}
	if !u.IsAbs() {
		// Relative URL — the browser resolves it against the iframe's
		// URL (which is already under the proxy), so leave it alone.
		return raw, false
	}
	if u.Host != upstream.Host || u.Scheme != upstream.Scheme {
		// Different host entirely — leave it alone (could be a link to
		// an external IdP or a CDN asset) so the browser can resolve it
		// directly. The CSP-governed iframe sandbox will gate what runs.
		return raw, false
	}
	// Absolute URL pointing at the upstream — rewrite it to the
	// proxy's URL so the browser can follow it.
	rewritten := "/api/products/" + productName + "/proxy" + u.Path
	if u.RawQuery != "" {
		rewritten += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		rewritten += "#" + u.Fragment
	}
	return rewritten, true
}

// stripCookieDomain removes a Domain= attribute from a Set-Cookie value
// so the cookie's host defaults to the request's host — the Factory's
// hostname — which the browser will then send back through the proxy. A
// Domain attribute pinned to the upstream's hostname would be rejected.
//
// Set-Cookie syntax: name=value; Attr=val; Domain=... We split on "; "
// (the canonical separator) and drop the Domain attribute case-
// insensitively. We don't parse strictly per RFC 6265 (a real cookie
// parser would) because the variation in the wild is small and this is a
// straight strip, not a re-emit.
func stripCookieDomain(c string) string {
	parts := strings.Split(c, ";")
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if strings.HasPrefix(strings.ToLower(t), "domain=") {
			continue
		}
		kept = append(kept, p)
	}
	return strings.Join(kept, ";")
}

// mergeSessionCookie combines the browser-sent Cookie header (existing) with
// the Factory's authoritative session cookie (session, a "name=value" pair),
// so the upstream receives exactly ONE value for the session cookie — ours.
// Any cookie in existing whose name matches the session cookie's name is
// dropped: it's a stale copy the browser accumulated because the base proxy
// strips Set-Cookie Domain (so the upstream's cookie gets stored under the
// Factory's host and sent back here). All other browser cookies are preserved
// in order and the session cookie is appended last. Empty existing yields just
// the session cookie. Cookie names are matched case-sensitively per RFC 6265.
func mergeSessionCookie(existing, session string) string {
	if existing == "" {
		return session
	}
	name := session
	if i := strings.IndexByte(session, '='); i >= 0 {
		name = session[:i]
	}
	parts := strings.Split(existing, ";")
	kept := make([]string, 0, len(parts)+1)
	for _, part := range parts {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		cookieName := p
		if i := strings.IndexByte(p, '='); i >= 0 {
			cookieName = p[:i]
		}
		if cookieName == name {
			continue // drop the browser's (possibly stale) same-named copy
		}
		kept = append(kept, p)
	}
	kept = append(kept, session)
	return strings.Join(kept, "; ")
}

// forwardingHeaderStripper wraps a RoundTripper and removes every
// forwarding-identifying header from the outgoing request before it
// reaches the wire. This is the only reliable place to strip them:
// ReverseProxy.ServeHTTP re-adds X-Forwarded-For from RemoteAddr AFTER
// the Director runs, so Director-side deletion is silently overridden.
// The Transport is the last stop before the network — stripping here
// actually sticks.
//
// The wrapped request is already a clone (ReverseProxy does
// req.Clone(ctx) before calling the Director), so mutating its headers
// here does not affect the original inbound request.
type forwardingHeaderStripper struct {
	base http.RoundTripper
}

func (f *forwardingHeaderStripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone the request's headers before mutating — the http.RoundTripper
	// contract says "RoundTrip should not modify the request, except for
	// consuming and closing the Request's Body." In practice ReverseProxy
	// passes us a clone, but being explicit avoids a future refactor
	// breaking the contract silently.
	req2 := req.Clone(req.Context())
	for _, h := range []string{
		"X-Forwarded-For",
		"X-Forwarded-Host",
		"X-Forwarded-Proto",
		"X-Forwarded-Server",
		"X-Forwarded-Port",
		"X-Real-IP",
		"Forwarded",
	} {
		req2.Header.Del(h)
	}
	return f.base.RoundTrip(req2)
}

// ─── Per-product session cache (auto-login) ──────────────────────────────────

// sessionCache holds one upstream session cookie per product, keyed by
// product name. When a product declares DashboardAuthToken, the proxy
// trades that token for a session cookie at DashboardAuthLogin (default
// /api/auth/login) on the first proxied request, then injects the
// cookie into every subsequent proxied request for that product — so
// the human never sees the product's own login wall. The cached cookie
// has a short TTL (default 1h): if it expires or the upstream starts
// 401ing, the proxy re-logs-in transparently.
//
// Security model:
//   - The token lives in the Factory's DB; it never reaches the
//     browser, never appears in logs, and is only sent to the upstream
//     over the internal Docker network (the proxy calls the upstream
//     directly, not the browser).
//   - The session cookie is per-product and lives only in the proxy's
//     process memory; it's not written to disk anywhere.
//   - On Factory restart, the cache is cold; the first request for
//     each product re-triggers a login. No state to migrate.
//   - The cache is bounded by the number of LIVE products with a
//     DashboardAuthToken, which is a single-digit number in practice.
//
// OpenAlice (the reference product this was built for) accepts
// POST /api/auth/login with {"token":"<first-run admin token>"} and
// returns Set-Cookie: alice_session=<opaque>; Max-Age=604799. The
// proxy extracts the first Set-Cookie value as the session cookie —
// it doesn't care what the cookie's name is, just that one exists.
//
// Other auth-shaped products: the default login path is /api/auth/login
// and the default request body is {"token":"<value>"}. Products with a
// different shape can set dashboard_auth_login to override the path;
// body shape is fixed (extending it to a template would be a future
// improvement, YAGNI until we have a second product that needs it).
type sessionCache struct {
	logger *zap.Logger
	mu     sync.Mutex
	chars  map[string]cachedSession // product name → session
}

type cachedSession struct {
	cookie    string // raw "name=value" cookie pair, e.g. "alice_session=abc123"
	expiresAt time.Time
	// loginFailedAt, if non-zero, records the last time a login attempt
	// failed. While backoff is in effect (loginBackoff window), new
	// requests for the session skip the login attempt and just forward
	// without a cookie — a misconfigured token shouldn't take the proxy
	// down or hammer the upstream with retries.
	loginFailedAt time.Time
}

// loginBackoff gates how often the cache will retry a failed login for
// a given product. Generous because the operator fixes the token in the
// wizard, not by poking the cache.
const loginBackoff = 5 * time.Minute

// sessionTTL bounds how long a cached cookie is trusted. OpenAlice's
// session is 7 days, but the proxy doesn't peek at Max-Age — a single
// TTL for everything is simpler and keeps the cache from going stale
// across long-running products. The cookie is revalidated lazily: if
// the upstream starts returning 401, the proxy re-logs in.
const sessionTTL = 1 * time.Hour

// loginClient times out the upstream login probe — a slow upstream
// login shouldn't hang the human's browser request. The login is
// best-effort (we fall back to proxying without a cookie if it fails),
// so a 5s budget is plenty.
var loginClient = &http.Client{Timeout: 5 * time.Second}

func newSessionCache(logger *zap.Logger) *sessionCache {
	return &sessionCache{logger: logger, chars: make(map[string]cachedSession)}
}

// cookieFor returns the cached session cookie for productName, doing
// a fresh login if the cache is cold, expired, or marked for re-login.
// Returns "" when the product has no token, when login fails, or when
// the cache is in backoff after a recent failure — in all those cases
// the proxy simply forwards the request without a session cookie (the
// upstream will likely 401, but the human's browser then sees the
// product's own "invalid session" UI rather than the Factory hanging).
func (c *sessionCache) cookieFor(productName string, p *db.Product, upstream *url.URL) string {
	if p.DashboardAuthToken == "" {
		return ""
	}
	c.mu.Lock()
	cached, ok := c.chars[productName]
	c.mu.Unlock()
	if ok && time.Now().Before(cached.expiresAt) && cached.cookie != "" {
		return cached.cookie
	}
	// Backoff: don't retry a failing login more often than loginBackoff.
	// This keeps a misconfigured token from drowning the upstream in
	// POSTs while the operator investigates.
	if ok && cached.cookie == "" && !cached.loginFailedAt.IsZero() && time.Since(cached.loginFailedAt) < loginBackoff {
		return ""
	}
	cookie, ok := c.login(productName, p, upstream)
	c.mu.Lock()
	defer c.mu.Unlock()
	if ok {
		c.chars[productName] = cachedSession{cookie: cookie, expiresAt: time.Now().Add(sessionTTL)}
		return cookie
	}
	// Record the failure so backoff kicks in. A previous good cookie is
	// wiped — we don't want to re-use a cookie we already doubt.
	c.chars[productName] = cachedSession{loginFailedAt: time.Now()}
	return ""
}

// invalidate forces a re-login on the next request — called when the
// upstream returns 401/403 on a request we previously had a cookie for.
// If the upstream is strict and the cookie was minted wrong, the next
// request re-logs in; if login is also failing, backoff governs.
func (c *sessionCache) invalidate(productName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.chars[productName]; ok {
		delete(c.chars, productName)
	}
}

// login POSTs the product's token to its DashboardAuthLogin path on
// the upstream and returns the first Set-Cookie value as "name=value".
// On any failure (non-2xx, no Set-Cookie, network error) it returns
// (_, false) and the caller records the failure for backoff.
func (c *sessionCache) login(productName string, p *db.Product, upstream *url.URL) (string, bool) {
	loginPath := p.DashboardAuthLogin
	if loginPath == "" {
		loginPath = "/api/auth/login"
	}
	// Build the login URL from the upstream's scheme/host so the request
	// is always same-origin with the dashboard the proxy is forwarding
	// to — a product that holds the session cookie to a specific Host
	// (OpenAlice's Koa session does) gets the right one.
	loginURL := *upstream
	loginURL.Path = loginPath
	loginURL.RawPath = ""
	loginURL.RawQuery = ""
	loginURL.Fragment = ""
	body := fmt.Sprintf(`{"token":%q}`, p.DashboardAuthToken)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, loginURL.String(), strings.NewReader(body))
	if err != nil {
		c.logger.Warn("proxy.login_build_failed",
			zap.String("product", productName), zap.Error(err))
		return "", false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Host = upstream.Host // product app ties session to its own Host
	resp, err := loginClient.Do(req)
	if err != nil {
		c.logger.Warn("proxy.login_request_failed",
			zap.String("product", productName), zap.Error(err))
		return "", false
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.logger.Warn("proxy.login_non_2xx",
			zap.String("product", productName), zap.Int("status", resp.StatusCode))
		return "", false
	}
	// Extract the first Set-Cookie's name=value pair. We don't parse
	// attributes (Path/Domain/HttpOnly/etc.) because we're going to
	// re-inject this as a Cookie header value ourselves — the upstream's
	// attribute decisions don't apply to a synthetic Cookie header on a
	// different request.
	for _, sc := range resp.Header.Values("Set-Cookie") {
		// Set-Cookie is "name=value; Attr=val; ...". The pair is the
		// first chunk before any ";". Trim whitespace just in case.
		if i := strings.IndexByte(sc, ';'); i >= 0 {
			sc = sc[:i]
		}
		sc = strings.TrimSpace(sc)
		if sc != "" && strings.Contains(sc, "=") {
			return sc, true
		}
	}
	c.logger.Warn("proxy.login_no_set_cookie", zap.String("product", productName))
	return "", false
}

// (no EOF marker — file ends here)
