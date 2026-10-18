// Command adminctl grants and revokes the admin role.
//
//	adminctl [-project ID] grant  <email>   set custom claim admin=true
//	adminctl [-project ID] revoke <email>   remove it and sign the user out everywhere
//	adminctl [-project ID] list             list admins (claim and config)
//	adminctl [-project ID] show   <email>   one user's account and role status
//
// It uses the Firebase Admin SDK with Application Default Credentials (run
// `gcloud auth application-default login` first). Set
// FIREBASE_AUTH_EMULATOR_HOST to manage the local emulator instead.
//
// Admins listed in ADMIN_EMAILS (the stack's adminEmails config) are shown
// but can't be revoked here: remove them from the config and redeploy.
//
// Role changes reach a signed-in user when their ID token refreshes (within
// an hour, or right away on sign-in). Revocation is immediate: revoke also
// invalidates the user's sessions, and the API re-checks admin tokens.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	firebase "firebase.google.com/go/v4"
	fbauth "firebase.google.com/go/v4/auth"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/example/asyncsvc/internal/auth"
)

func main() {
	project := flag.String("project", os.Getenv("GCP_PROJECT"), "GCP / Firebase project ID (default $GCP_PROJECT)")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), `Usage: adminctl [-project ID] <command> [email]

Commands:
  grant  <email>   give the admin role (custom claim admin=true)
  revoke <email>   remove the admin role and sign the user out everywhere
  list             list admins (custom claim, plus ADMIN_EMAILS config)
  show   <email>   show a user's account and role status

Flags:
`)
		flag.PrintDefaults()
	}
	flag.Parse()

	if err := run(*project, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "adminctl:", err)
		os.Exit(1)
	}
}

func run(project string, args []string) error {
	if len(args) == 0 {
		flag.Usage()
		return errors.New("missing command")
	}
	if project == "" {
		return errors.New("set -project or GCP_PROJECT")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	client, err := newClient(ctx, project)
	if err != nil {
		return err
	}
	configAdmins := configAdmins()

	cmd, rest := args[0], args[1:]
	email := ""
	if cmd != "list" {
		if len(rest) != 1 {
			return fmt.Errorf("%s needs exactly one email", cmd)
		}
		email = strings.TrimSpace(rest[0])
	}

	switch cmd {
	case "grant":
		return grant(ctx, client, email)
	case "revoke":
		return revoke(ctx, client, email, configAdmins)
	case "show":
		return show(ctx, client, email, configAdmins)
	case "list":
		return list(ctx, client, configAdmins)
	default:
		flag.Usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func newClient(ctx context.Context, project string) (*fbauth.Client, error) {
	var opts []option.ClientOption
	if os.Getenv("FIREBASE_AUTH_EMULATOR_HOST") != "" {
		opts = append(opts, option.WithoutAuthentication())
	}
	app, err := firebase.NewApp(ctx, &firebase.Config{ProjectID: project}, opts...)
	if err != nil {
		return nil, fmt.Errorf("firebase app: %w", err)
	}
	return app.Auth(ctx)
}

// configAdmins are the emails in ADMIN_EMAILS (the deployed adminEmails).
func configAdmins() map[string]bool {
	out := map[string]bool{}
	for _, e := range strings.Split(os.Getenv("ADMIN_EMAILS"), ",") {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			out[e] = true
		}
	}
	return out
}

func lookup(ctx context.Context, c *fbauth.Client, email string) (*fbauth.UserRecord, error) {
	u, err := c.GetUserByEmail(ctx, email)
	if fbauth.IsUserNotFound(err) {
		return nil, fmt.Errorf("no account for %s (they need to sign up first)", email)
	}
	return u, err
}

func grant(ctx context.Context, c *fbauth.Client, email string) error {
	u, err := lookup(ctx, c, email)
	if err != nil {
		return err
	}
	if auth.HasAdminClaim(u.CustomClaims) {
		fmt.Printf("%s is already an admin.\n", email)
		return nil
	}
	if err := c.SetCustomUserClaims(ctx, u.UID, auth.WithAdmin(u.CustomClaims, true)); err != nil {
		return fmt.Errorf("set claims: %w", err)
	}
	fmt.Printf("Granted admin to %s (uid %s).\n", email, u.UID)
	if !u.EmailVerified {
		fmt.Println("Note: their email isn't verified. The claim works regardless, but they won't get report emails until it is.")
	}
	fmt.Println("It applies when their sign-in refreshes: within an hour, or right away if they sign out and back in.")
	return nil
}

func revoke(ctx context.Context, c *fbauth.Client, email string, configAdmins map[string]bool) error {
	u, err := lookup(ctx, c, email)
	if err != nil {
		return err
	}
	had := auth.HasAdminClaim(u.CustomClaims)
	if had {
		if err := c.SetCustomUserClaims(ctx, u.UID, auth.WithAdmin(u.CustomClaims, false)); err != nil {
			return fmt.Errorf("set claims: %w", err)
		}
		// Invalidate existing sessions so the old admin token stops working
		// now. The API re-checks revocation for admin tokens.
		if err := c.RevokeRefreshTokens(ctx, u.UID); err != nil {
			return fmt.Errorf("revoke sessions: %w", err)
		}
		fmt.Printf("Revoked admin from %s and signed them out everywhere.\n", email)
	} else {
		fmt.Printf("%s doesn't have the admin claim.\n", email)
	}
	if configAdmins[strings.ToLower(u.Email)] {
		fmt.Printf("Warning: %s is still an admin through the adminEmails config. Remove it there and redeploy:\n", email)
		fmt.Println("  cd infra && pulumi config set adminEmails <remaining,emails>")
		if !had {
			return errors.New("nothing revoked")
		}
	}
	return nil
}

func show(ctx context.Context, c *fbauth.Client, email string, configAdmins map[string]bool) error {
	u, err := lookup(ctx, c, email)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "uid\t%s\n", u.UID)
	fmt.Fprintf(tw, "email\t%s (verified: %t)\n", u.Email, u.EmailVerified)
	fmt.Fprintf(tw, "disabled\t%t\n", u.Disabled)
	fmt.Fprintf(tw, "admin (claim)\t%t\n", auth.HasAdminClaim(u.CustomClaims))
	fmt.Fprintf(tw, "admin (config)\t%t\n", configAdmins[strings.ToLower(u.Email)] && u.EmailVerified)
	if u.UserMetadata != nil && u.UserMetadata.LastLogInTimestamp > 0 {
		fmt.Fprintf(tw, "last sign-in\t%s\n", time.UnixMilli(u.UserMetadata.LastLogInTimestamp).UTC().Format(time.RFC3339))
	}
	return tw.Flush()
}

func list(ctx context.Context, c *fbauth.Client, configAdmins map[string]bool) error {
	type row struct{ email, uid, via string }
	var rows []row
	seen := map[string]bool{}

	it := c.Users(ctx, "")
	for {
		u, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return fmt.Errorf("list users: %w", err)
		}
		email := strings.ToLower(u.Email)
		var via []string
		if auth.HasAdminClaim(u.CustomClaims) {
			via = append(via, "claim")
		}
		if configAdmins[email] {
			if u.EmailVerified {
				via = append(via, "config")
			} else {
				via = append(via, "config (email not verified: not active)")
			}
		}
		if len(via) > 0 {
			rows = append(rows, row{u.Email, u.UID, strings.Join(via, ", ")})
			seen[email] = true
		}
	}
	for e := range configAdmins {
		if !seen[e] {
			rows = append(rows, row{e, "-", "config (no account yet)"})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].email < rows[j].email })

	if len(rows) == 0 {
		fmt.Println("No admins.")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "EMAIL\tUID\tADMIN VIA")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.email, r.uid, r.via)
	}
	return tw.Flush()
}
