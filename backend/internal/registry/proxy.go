package registry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
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
//                                  (the query string is on r.URL, not in rest)
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

// (no EOF marker — file ends here)
