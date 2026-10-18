// Package version reports what build is running.
//
//   - Commit is the git commit the binaries were built from, set at build
//     time: go build -ldflags "-X github.com/example/asyncsvc/internal/version.Commit=<sha>"
//   - Image is the container image (repo@sha256:...) the deployment runs,
//     set by the deployment through the IMAGE environment variable.
package version

import (
	"net/http"
	"os"
	"runtime"
	"strings"

	"github.com/example/asyncsvc/internal/httpx"
)

var Commit = "dev"

type Info struct {
	Commit      string `json:"commit"`
	Image       string `json:"image,omitempty"`
	ImageDigest string `json:"image_digest,omitempty"`
	Go          string `json:"go"`
}

func Get() Info {
	img := os.Getenv("IMAGE")
	info := Info{Commit: Commit, Image: img, Go: runtime.Version()}
	if _, digest, ok := strings.Cut(img, "@"); ok {
		info.ImageDigest = digest
	}
	return info
}

// Handler serves GET /version.
func Handler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, Get())
}
