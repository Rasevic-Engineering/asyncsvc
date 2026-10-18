package api

import (
	"net/http"
	"net/http/httputil"
	"net/url"
)

// firebaseAuthProxy forwards Firebase Auth's sign-in helper paths (/__/auth/*
// and /__/firebase/*) to <project>.firebaseapp.com, so the app can use its
// own domain as the Firebase authDomain.
//
// Without this, the Google sign-in popup runs on firebaseapp.com, a different
// site from the app, and browsers that block third-party storage (Safari,
// Firefox, Chrome with tracking protection) can break the flow. This is
// Firebase's documented "proxy auth requests" setup.
func firebaseAuthProxy(handlerHost string) http.Handler {
	target := &url.URL{Scheme: "https", Host: handlerHost}
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Host = handlerHost
			// Firebase only needs the path and query; don't forward the
			// app's cookies or auth headers to another origin.
			r.Out.Header.Del("Cookie")
			r.Out.Header.Del("Authorization")
		},
	}
}
