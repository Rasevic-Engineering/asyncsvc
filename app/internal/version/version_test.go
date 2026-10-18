package version

import "testing"

func TestGet(t *testing.T) {
	t.Setenv("IMAGE", "us-central1-docker.pkg.dev/p/r/app@sha256:abc123")
	Commit = "0123456789abcdef"
	v := Get()
	if v.Commit != "0123456789abcdef" || v.ImageDigest != "sha256:abc123" || v.Image == "" {
		t.Fatalf("%+v", v)
	}
	t.Setenv("IMAGE", "")
	if v := Get(); v.ImageDigest != "" || v.Image != "" {
		t.Fatalf("no image: %+v", v)
	}
}
