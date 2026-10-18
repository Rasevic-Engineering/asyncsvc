// Package auth verifies Firebase ID tokens and enforces basic authorization.
//
// Roles:
//   - user:  any signed-in user. Can create reports and read their own.
//   - admin: token has the custom claim admin=true, or a verified email listed
//     in ADMIN_EMAILS. Can read every report and publish analytics events.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	firebase "firebase.google.com/go/v4"
	fbauth "firebase.google.com/go/v4/auth"
	"google.golang.org/api/option"

	"github.com/example/asyncsvc/internal/httpx"
)

type User struct {
	UID           string `json:"uid"`
	Email         string `json:"email,omitempty"`
	EmailVerified bool   `json:"email_verified"`
	Admin         bool   `json:"admin"`
}

// Verifier turns a bearer token into a User.
type Verifier interface {
	Verify(ctx context.Context, token string) (User, error)
}

// Firebase verifies Firebase Auth / Identity Platform ID tokens. When
// FIREBASE_AUTH_EMULATOR_HOST is set, the Admin SDK accepts the emulator's
// unsigned tokens instead (local development only).
type Firebase struct {
	client      *fbauth.Client
	adminEmails map[string]bool
	// Treat unverified emails as verified. Only for the local emulator,
	// where email/password accounts can't easily be verified.
	trustUnverified bool
}

func NewFirebase(ctx context.Context, projectID string, adminEmails []string, trustUnverified bool) (*Firebase, error) {
	var opts []option.ClientOption
	if os.Getenv("FIREBASE_AUTH_EMULATOR_HOST") != "" {
		opts = append(opts, option.WithoutAuthentication())
	}
	app, err := firebase.NewApp(ctx, &firebase.Config{ProjectID: projectID}, opts...)
	if err != nil {
		return nil, fmt.Errorf("firebase app: %w", err)
	}
	client, err := app.Auth(ctx)
	if err != nil {
		return nil, fmt.Errorf("firebase auth client: %w", err)
	}
	f := &Firebase{client: client, adminEmails: map[string]bool{}, trustUnverified: trustUnverified}
	for _, e := range adminEmails {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			f.adminEmails[e] = true
		}
	}
	return f, nil
}

func (f *Firebase) Verify(ctx context.Context, raw string) (User, error) {
	// Signature, expiry, issuer and audience (the project) are all checked.
	t, err := f.client.VerifyIDToken(ctx, raw)
	if err != nil {
		return User{}, err
	}
	// Tokens carrying the admin claim are also checked for revocation, so
	// `adminctl revoke` takes effect immediately rather than when the
	// token expires (up to an hour). It's one lookup, on admin requests
	// only; regular users' tokens aren't checked.
	if HasAdminClaim(t.Claims) {
		if t, err = f.client.VerifyIDTokenAndCheckRevoked(ctx, raw); err != nil {
			return User{}, err
		}
	}
	u := User{UID: t.UID}
	u.Email, _ = t.Claims["email"].(string)
	verified, _ := t.Claims["email_verified"].(bool)
	u.EmailVerified = verified || (f.trustUnverified && u.Email != "")
	u.Admin = HasAdminClaim(t.Claims) || (u.EmailVerified && f.adminEmails[strings.ToLower(u.Email)])
	return u, nil
}

// Static maps fixed tokens to users. For tests.
type Static map[string]User

func (s Static) Verify(_ context.Context, token string) (User, error) {
	if u, ok := s[token]; ok {
		return u, nil
	}
	return User{}, errors.New("unknown token")
}

type ctxKey struct{}

func FromContext(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(ctxKey{}).(User)
	return u, ok
}

func WithUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

// Require rejects requests without a valid "Authorization: Bearer <token>".
func Require(v Verifier, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			w.Header().Set("WWW-Authenticate", `Bearer`)
			httpx.Error(w, http.StatusUnauthorized, "sign in required")
			return
		}
		u, err := v.Verify(r.Context(), token)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			httpx.Error(w, http.StatusUnauthorized, "invalid or expired token")
			return
		}
		next(w, r.WithContext(WithUser(r.Context(), u)))
	}
}

// RequireAdmin must be wrapped by Require.
func RequireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if u, ok := FromContext(r.Context()); !ok || !u.Admin {
			httpx.Error(w, http.StatusForbidden, "admin access required")
			return
		}
		next(w, r)
	}
}
