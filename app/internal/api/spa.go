package api

import (
	"crypto/sha256"
	"encoding/base64"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/example/asyncsvc/internal/httpx"
)

// spaHandler serves the statically exported Next.js app from dir.
//
// Path resolution mirrors how `next build` with output: "export" lays out
// files: /login -> login.html, /x/ -> x/index.html. Unknown paths get the
// exported 404 page. Unknown /v1/ paths stay JSON 404s.
//
// Next inlines small <script> blocks in every page. Instead of allowing
// 'unsafe-inline', the handler hashes those scripts at startup and allows
// exactly them in each page's Content-Security-Policy.
func spaHandler(dir string, web WebConfig) http.Handler {
	hashes := inlineScriptHashes(dir)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := path.Clean("/" + r.URL.Path)
		if p == "/v1" || strings.HasPrefix(p, "/v1/") {
			httpx.Error(w, http.StatusNotFound, "not found")
			return
		}

		file, status := resolve(dir, p)
		if file == "" {
			http.NotFound(w, r)
			return
		}

		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Content-Security-Policy", contentSecurityPolicy(web, hashes[file]))
		if strings.HasPrefix(p, "/_next/static/") || strings.HasPrefix(p, "/assets/") {
			// Fingerprinted file names, so they can be cached forever.
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			h.Set("Cache-Control", "no-cache")
		}

		if status == http.StatusOK {
			http.ServeFile(w, r, file)
			return
		}
		// http.ServeFile can't set a status, so write the 404 page directly.
		b, err := os.ReadFile(file)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		h.Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write(b)
	})
}

// resolve maps a clean URL path to a file under dir.
func resolve(dir, p string) (file string, status int) {
	base := filepath.Join(dir, filepath.FromSlash(p))
	for _, candidate := range []string{base, base + ".html", filepath.Join(base, "index.html")} {
		if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
			return candidate, http.StatusOK
		}
	}
	if f := filepath.Join(dir, "404.html"); exists(f) {
		return f, http.StatusNotFound
	}
	// A plain SPA without a 404 page: let the client router handle it.
	if f := filepath.Join(dir, "index.html"); exists(f) {
		return f, http.StatusOK
	}
	return "", http.StatusNotFound
}

func exists(f string) bool {
	fi, err := os.Stat(f)
	return err == nil && !fi.IsDir()
}

var (
	scriptTag = regexp.MustCompile(`(?is)<script([^>]*)>(.*?)</script>`)
	srcAttr   = regexp.MustCompile(`(?i)\ssrc\s*=`)
)

// inlineScriptHashes returns, per HTML file, the CSP hash sources of its
// inline scripts.
func inlineScriptHashes(dir string) map[string][]string {
	out := map[string][]string{}
	_ = filepath.WalkDir(dir, func(f string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(f, ".html") {
			return nil
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return nil
		}
		seen := map[string]bool{}
		for _, m := range scriptTag.FindAllSubmatch(b, -1) {
			if srcAttr.Match(m[1]) || len(m[2]) == 0 {
				continue
			}
			sum := sha256.Sum256(m[2])
			h := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
			if !seen[h] {
				seen[h] = true
				out[f] = append(out[f], h)
			}
		}
		return nil
	})
	slog.Info("web app loaded", "dir", dir, "pages", len(out))
	return out
}

// contentSecurityPolicy allows only this origin plus what Firebase Auth needs:
// its token endpoints, the Google sign-in helper script, and the auth domain's
// iframe (and the emulator when running locally). scriptHashes are the inline
// scripts of the page being served.
func contentSecurityPolicy(web WebConfig, scriptHashes []string) string {
	script := append([]string{"'self'", "https://apis.google.com"}, scriptHashes...)
	connect := []string{"'self'", "https://identitytoolkit.googleapis.com", "https://securetoken.googleapis.com"}
	frame := []string{}
	if d := web.Firebase.AuthDomain; d != "" {
		frame = append(frame, "https://"+d)
	}
	if web.AuthEmulatorURL != "" {
		connect = append(connect, web.AuthEmulatorURL)
		frame = append(frame, web.AuthEmulatorURL)
	}
	if len(frame) == 0 {
		frame = append(frame, "'none'")
	}
	return strings.Join([]string{
		"default-src 'self'",
		"script-src " + strings.Join(script, " "),
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data:",
		"connect-src " + strings.Join(connect, " "),
		"frame-src " + strings.Join(frame, " "),
		"frame-ancestors 'none'",
		"base-uri 'self'",
		"form-action 'self'",
	}, "; ")
}
