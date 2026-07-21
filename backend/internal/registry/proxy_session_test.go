package registry

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"

	"github.com/wrcron2/market-ai-factory/backend/internal/db"
)

// loginServer is a stand-in product login endpoint. It counts how many times
// it's hit (to prove caching/backoff) and records the last path it saw (to
// prove the login-path override). The response is configurable per test.
type loginServer struct {
	srv      *httptest.Server
	hits     int32
	lastPath string
}

func newLoginServer(t *testing.T, handler http.HandlerFunc) (*loginServer, *url.URL) {
	t.Helper()
	ls := &loginServer{}
	ls.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&ls.hits, 1)
		ls.lastPath = r.URL.Path
		handler(w, r)
	}))
	t.Cleanup(ls.srv.Close)
	u, err := url.Parse(ls.srv.URL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}
	return ls, u
}

func (ls *loginServer) hitCount() int32 { return atomic.LoadInt32(&ls.hits) }

func newCache() *sessionCache { return newSessionCache(zap.NewNop()) }

// setsCookie writes one Set-Cookie with attributes, to confirm login extracts
// just the name=value pair.
func setsCookie(cookie string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", cookie)
		w.WriteHeader(http.StatusOK)
	}
}

// TestCookieForNoTokenReturnsEmpty — a product with no auth token never
// triggers a login; the proxy just forwards without a session cookie.
func TestCookieForNoTokenReturnsEmpty(t *testing.T) {
	ls, up := newLoginServer(t, setsCookie("sess=abc"))
	c := newCache()
	p := &db.Product{Name: "p", DashboardAuthToken: ""}
	if got := c.cookieFor("p", p, up); got != "" {
		t.Fatalf("want empty cookie for token-less product, got %q", got)
	}
	if ls.hitCount() != 0 {
		t.Fatalf("token-less product must not hit the login endpoint, hits=%d", ls.hitCount())
	}
}

// TestCookieForLoginSuccessCachesCookie — the first request logs in and
// returns the cookie; the second request is served from cache without a
// second login round-trip.
func TestCookieForLoginSuccessCachesCookie(t *testing.T) {
	ls, up := newLoginServer(t, setsCookie("alice_session=xyz789; Max-Age=604799; Path=/; HttpOnly"))
	c := newCache()
	p := &db.Product{Name: "p", DashboardAuthToken: "tok"}

	got := c.cookieFor("p", p, up)
	if got != "alice_session=xyz789" {
		t.Fatalf("want just the name=value pair, got %q", got)
	}
	if got2 := c.cookieFor("p", p, up); got2 != got {
		t.Fatalf("cached call returned %q, want %q", got2, got)
	}
	if ls.hitCount() != 1 {
		t.Fatalf("second call should be cache-served, login hits=%d want 1", ls.hitCount())
	}
}

// TestCookieForBacksOffAfterFailedLogin — a failing login (non-2xx) returns
// empty and records the failure; an immediate retry is suppressed by backoff
// rather than hammering the upstream.
func TestCookieForBacksOffAfterFailedLogin(t *testing.T) {
	ls, up := newLoginServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	c := newCache()
	p := &db.Product{Name: "p", DashboardAuthToken: "tok"}

	if got := c.cookieFor("p", p, up); got != "" {
		t.Fatalf("failed login should yield empty cookie, got %q", got)
	}
	if got := c.cookieFor("p", p, up); got != "" {
		t.Fatalf("second call should still be empty, got %q", got)
	}
	if ls.hitCount() != 1 {
		t.Fatalf("backoff should suppress the retry, login hits=%d want 1", ls.hitCount())
	}
}

// TestInvalidateForcesReLogin — after a 401/403 on a proxied request the
// cache entry is invalidated; the next cookieFor re-logs in.
func TestInvalidateForcesReLogin(t *testing.T) {
	ls, up := newLoginServer(t, setsCookie("sess=v1"))
	c := newCache()
	p := &db.Product{Name: "p", DashboardAuthToken: "tok"}

	if got := c.cookieFor("p", p, up); got != "sess=v1" {
		t.Fatalf("initial login: got %q", got)
	}
	c.invalidate("p")
	if got := c.cookieFor("p", p, up); got != "sess=v1" {
		t.Fatalf("post-invalidate login: got %q", got)
	}
	if ls.hitCount() != 2 {
		t.Fatalf("invalidate should force a second login, hits=%d want 2", ls.hitCount())
	}
}

// TestLoginUsesCustomLoginPath — DashboardAuthLogin overrides the default
// /api/auth/login path.
func TestLoginUsesCustomLoginPath(t *testing.T) {
	ls, up := newLoginServer(t, setsCookie("sess=v1"))
	c := newCache()
	p := &db.Product{Name: "p", DashboardAuthToken: "tok", DashboardAuthLogin: "/custom/signin"}

	c.cookieFor("p", p, up)
	if ls.lastPath != "/custom/signin" {
		t.Fatalf("login POSTed to %q, want the override /custom/signin", ls.lastPath)
	}
}

// TestLoginDefaultsPathWhenUnset — an empty DashboardAuthLogin falls back to
// /api/auth/login (OpenAlice's shape).
func TestLoginDefaultsPathWhenUnset(t *testing.T) {
	ls, up := newLoginServer(t, setsCookie("sess=v1"))
	c := newCache()
	p := &db.Product{Name: "p", DashboardAuthToken: "tok"}

	c.cookieFor("p", p, up)
	if ls.lastPath != "/api/auth/login" {
		t.Fatalf("login POSTed to %q, want the default /api/auth/login", ls.lastPath)
	}
}

// TestLoginNoSetCookieReturnsEmpty — a 200 login that sets no cookie is a
// failure: there's nothing to inject, so cookieFor returns empty.
func TestLoginNoSetCookieReturnsEmpty(t *testing.T) {
	_, up := newLoginServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // 200 but no Set-Cookie
	})
	c := newCache()
	p := &db.Product{Name: "p", DashboardAuthToken: "tok"}

	if got := c.cookieFor("p", p, up); got != "" {
		t.Fatalf("no Set-Cookie should yield empty, got %q", got)
	}
}

// TestMergeSessionCookieFactoryWins is the QA #1 regression guard. The proxy
// caches the authoritative session cookie; the browser may hold a STALE copy
// of the same-named cookie (the base proxy strips Set-Cookie Domain, so the
// browser stores the upstream's session cookie under the Factory's host and
// sends it back). If the browser's stale value were sent alongside — worse,
// ordered first — a first-match upstream (Koa's cookie parser) would keep
// authenticating against the expired value, and the cache's re-login could
// never take effect: a permanent 401 loop. The Factory's value must win, and
// unrelated cookies (theme, etc.) must survive.
func TestMergeSessionCookieFactoryWins(t *testing.T) {
	cases := []struct {
		name, existing, session, want string
	}{
		{"drops stale same-named, keeps others", "alice_session=STALE; theme=dark", "alice_session=FRESH", "theme=dark; alice_session=FRESH"},
		{"no same-named just appends", "theme=dark", "alice_session=FRESH", "theme=dark; alice_session=FRESH"},
		{"empty existing returns session", "", "alice_session=FRESH", "alice_session=FRESH"},
		{"only a stale same-named leaves just the fresh one", "alice_session=STALE", "alice_session=FRESH", "alice_session=FRESH"},
		{"cookie names are case-sensitive per RFC 6265", "Alice_Session=other", "alice_session=FRESH", "Alice_Session=other; alice_session=FRESH"},
		{"whitespace around pairs tolerated", "  a=1 ;  alice_session=STALE ; b=2 ", "alice_session=FRESH", "a=1; b=2; alice_session=FRESH"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mergeSessionCookie(c.existing, c.session); got != c.want {
				t.Fatalf("mergeSessionCookie(%q,%q) = %q, want %q", c.existing, c.session, got, c.want)
			}
		})
	}
}
