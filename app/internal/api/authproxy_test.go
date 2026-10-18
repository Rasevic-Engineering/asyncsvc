package api

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
)

func TestFirebaseAuthProxy(t *testing.T) {
	var got *http.Request
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.Write([]byte("firebase handler"))
	}))
	defer upstream.Close()

	// Same rewrite as production, but pointed at the plain-HTTP test server.
	u, _ := url.Parse(upstream.URL)
	p := firebaseAuthProxy(u.Host).(*httputil.ReverseProxy)
	rewrite := p.Rewrite
	p.Rewrite = func(r *httputil.ProxyRequest) {
		rewrite(r)
		r.Out.URL.Scheme = "http"
	}

	req := httptest.NewRequest("GET", "/__/auth/handler?apiKey=k", nil)
	req.Header.Set("Cookie", "session=secret")
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != 200 || rec.Body.String() != "firebase handler" {
		t.Fatalf("proxy response: %d %q", rec.Code, rec.Body.String())
	}
	if got.URL.Path != "/__/auth/handler" || got.URL.RawQuery != "apiKey=k" || got.Host != u.Host {
		t.Fatalf("upstream saw %s %s?%s", got.Host, got.URL.Path, got.URL.RawQuery)
	}
	if got.Header.Get("Cookie") != "" || got.Header.Get("Authorization") != "" {
		t.Fatalf("credentials leaked upstream: %v", got.Header)
	}
}

func TestAuthProxyRoutedBeforeWebApp(t *testing.T) {
	// Unreachable host: a 502 proves the request hit the proxy rather than
	// the web app's 404 page.
	s := &Server{FirebaseHandlerHost: "127.0.0.1:1", WebDir: t.TempDir()}
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/__/auth/iframe", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502 from proxy, got %d", rec.Code)
	}
}
