package auth

import "maps"

// ClaimAdmin is the Firebase custom claim that grants the admin role.
const ClaimAdmin = "admin"

// WithAdmin returns a copy of claims with the admin claim set or removed,
// leaving any other custom claims untouched. A nil result means "no custom
// claims" (Firebase clears them).
func WithAdmin(claims map[string]any, admin bool) map[string]any {
	out := maps.Clone(claims)
	if out == nil {
		out = map[string]any{}
	}
	if admin {
		out[ClaimAdmin] = true
	} else {
		delete(out, ClaimAdmin)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// HasAdminClaim reports whether custom claims grant admin.
func HasAdminClaim(claims map[string]any) bool {
	v, _ := claims[ClaimAdmin].(bool)
	return v
}
