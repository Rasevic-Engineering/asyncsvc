package auth

import "testing"

func TestWithAdmin(t *testing.T) {
	other := map[string]any{"team": "data"}

	got := WithAdmin(other, true)
	if !HasAdminClaim(got) || got["team"] != "data" {
		t.Fatalf("grant should keep other claims: %v", got)
	}
	if HasAdminClaim(other) {
		t.Fatal("input must not be modified")
	}
	if got := WithAdmin(got, false); HasAdminClaim(got) || got["team"] != "data" {
		t.Fatalf("revoke should keep other claims: %v", got)
	}
	if got := WithAdmin(map[string]any{"admin": true}, false); got != nil {
		t.Fatalf("revoking the only claim should clear claims, got %v", got)
	}
	if got := WithAdmin(nil, true); !HasAdminClaim(got) {
		t.Fatalf("grant on no claims: %v", got)
	}
	if HasAdminClaim(map[string]any{"admin": "true"}) {
		t.Fatal(`only a boolean true grants admin, not the string "true"`)
	}
}
