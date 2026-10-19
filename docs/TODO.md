# Improvements and TODOs

Known gaps and planned improvements, roughly in priority order within each section. Each item says what's missing today and what to change. The first item has a full implementation plan.

## Security and access

### IAM database authentication for human access

**Today:** people connect to Cloud SQL through the Cloud SQL Auth Proxy ([cloud-sql.md](cloud-sql.md)) as the **`app` user, with its password from Secret Manager**. That means:
- anyone who can read the secret gets full read/write access as the application;
- there's no per-person audit trail in Postgres, since everyone appears as `app`;
- offboarding someone doesn't revoke the password, so it has to be rotated.

**Goal:** people log in to Postgres **as their Google identity, with no password**, get **read-only by default**, and lose access the moment their IAM role is removed. The app keeps its password-based `app` user. Only human access changes.

**How it works:** Cloud SQL's IAM database authentication. The proxy, started with `--auto-iam-authn`, gets a short-lived OAuth token for your gcloud identity and uses it as the Postgres password. Cloud SQL checks the token against IAM, so nobody handles a password.

#### Steps

1. **Turn on the instance flag** in `infra/main.go`, under `sql.DatabaseInstanceSettingsArgs`:

   ```go
   DatabaseFlags: sql.DatabaseInstanceSettingsDatabaseFlagArray{
       &sql.DatabaseInstanceSettingsDatabaseFlagArgs{
           Name:  pulumi.String("cloudsql.iam_authentication"),
           Value: pulumi.String("on"),
       },
   },
   ```

   Changing database flags can restart the instance. Apply this in a quiet window and check `pulumi preview` for the impact.

2. **Create Postgres roles for permission levels** in a new migration, e.g. `app/internal/db/migrations/005_human_roles.sql`. It runs as `app`, which owns the tables, so it can grant on them:

   ```sql
   -- Permission groups for people. IAM users are granted membership; they
   -- get no privileges of their own.
   CREATE ROLE asyncsvc_readonly NOLOGIN;
   GRANT CONNECT ON DATABASE app TO asyncsvc_readonly;
   GRANT USAGE ON SCHEMA public TO asyncsvc_readonly;
   GRANT SELECT ON ALL TABLES IN SCHEMA public TO asyncsvc_readonly;
   -- Tables created by future migrations are readable too.
   ALTER DEFAULT PRIVILEGES FOR ROLE app IN SCHEMA public GRANT SELECT ON TABLES TO asyncsvc_readonly;

   -- For rare hands-on fixes (e.g. re-queueing a stuck report).
   CREATE ROLE asyncsvc_readwrite NOLOGIN IN ROLE asyncsvc_readonly;
   GRANT INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO asyncsvc_readwrite;
   ALTER DEFAULT PRIVILEGES FOR ROLE app IN SCHEMA public GRANT INSERT, UPDATE, DELETE ON TABLES TO asyncsvc_readwrite;
   ```

   This needs `app` to be allowed to create roles. Cloud SQL's built-in users have `CREATEROLE`; check with `\du` first.

3. **Add people in Pulumi.** Add a config list, e.g. `asyncsvc:dbHumans` with entries such as `andrej@rasevicengineering.com:readonly`, and create for each person:

   ```go
   // Postgres user named after the Google account.
   sql.NewUser(ctx, "db-human-"+slug, &sql.UserArgs{
       Instance: s.dbInstance.Name,
       Name:     pulumi.String(email),          // e.g. andrej@rasevicengineering.com
       Type:     pulumi.String("CLOUD_IAM_USER"),
   })
   // Allowed to log in to the instance with IAM, and to use the proxy.
   projects.NewIAMMember(ctx, "db-human-login-"+slug, &projects.IAMMemberArgs{
       Project: pulumi.String(s.project),
       Role:    pulumi.String("roles/cloudsql.instanceUser"),
       Member:  pulumi.String("user:" + email),
   })
   projects.NewIAMMember(ctx, "db-human-client-"+slug, &projects.IAMMemberArgs{
       Project: pulumi.String(s.project),
       Role:    pulumi.String("roles/cloudsql.client"),
       Member:  pulumi.String("user:" + email),
   })
   ```

   For teams, use a Google group with `Type: "CLOUD_IAM_GROUP"`, so membership is managed in Google Workspace rather than in config.

4. **Grant each person their permission level.** This happens inside Postgres, so Pulumi can't do it without a database connection. Run it once per person as `app`, or add it to a small `dbctl` CLI in the style of `adminctl`:

   ```sql
   GRANT asyncsvc_readonly TO "andrej@rasevicengineering.com";
   -- or, rarely:  GRANT asyncsvc_readwrite TO "andrej@rasevicengineering.com";
   ```

   The user name is the full email, in double quotes. For a service account it's the email without `.gserviceaccount.com`.

5. **Connect without a password:**

   ```bash
   cloud-sql-proxy --auto-iam-authn asyncsvc-dev:us-central1:postgres-1923e34 --port 5433
   ```

   ```bash
   psql "host=127.0.0.1 port=5433 user=andrej@rasevicengineering.com dbname=app"
   ```

   No `PGPASSWORD`. A permission error on `SELECT` means step 4 hasn't been done for that person.

6. **Lock down the app password.** Once people use IAM logins, only the API and worker service accounts need to read `asyncsvc-db-password`. Project owners can still read every secret, so for stronger separation move day-to-day human roles below Owner (e.g. Editor plus specific admin roles) and keep Owner for break-glass use.

7. **Turn on auditing.** Enable Cloud SQL **Data Access audit logs** for `cloudsql.googleapis.com` (who connected), and optionally the `pgaudit` extension (`cloudsql.enable_pgaudit=on`, then `CREATE EXTENSION pgaudit`) to log statements per IAM user.

8. **Update [cloud-sql.md](cloud-sql.md)** to make the IAM login the default, and keep the password method as break-glass only.

**Offboarding afterwards:** remove the person from `dbHumans`, or from the Google group, and deploy. Their IAM roles and Postgres user go away, and no secret needs rotating.

### Other security items

- [ ] **Google sign-in.** Code and infra are ready; create the OAuth client and set `googleOAuthClientId` / `googleOAuthClientSecret` (see the README).
- [ ] **Restrict who can sign up.** Sign-up is open to anyone. Options: allow only `@rasevicengineering.com` (an Identity Platform blocking function, or a check in the API), or turn off self-service sign-up and invite users from the Firebase console.
- [ ] **Make the load balancer the only way in.** The Cloud Run `run.app` URL is still public and bypasses the load balancer, and any future Cloud Armor rules with it. Set API ingress to `INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER` and point the CI smoke test at the custom domain.
- [ ] **Cloud Armor** on the load balancer: per-IP rate limits (especially `POST /v1/reports`, which costs BigQuery money) and the OWASP preconfigured rules.
- [ ] **Finer-grained roles** than user/admin (e.g. `viewer`, `analyst`, `admin`): a `role` claim plus a permission map in `internal/auth`, managed with `adminctl`.
- [ ] **Audit log** of who did what in the app (report requests, admin actions, role changes), e.g. as another `activity`-style topic into BigQuery.
- [ ] **Firestore revocation lag.** Firestore rules can't check revoked tokens, so a revoked admin can read the Activity feed for up to an hour. If that matters, mint short-lived custom tokens for the feed, or serve it through the API.
- [ ] **Private IP for Cloud SQL** (Private Service Connect or private services access, plus Direct VPC egress from Cloud Run). People then need a network path in: IAP TCP forwarding to a small VM, or a VPN.
- [ ] **Firebase App Check** on the web app, so only your site can call the API with valid Firebase tokens.

## Reliability and operations

- [ ] **Alerting** (Cloud Monitoring, in Pulumi):
  - dead-letter topic message count > 0;
  - oldest unacked message age on `reports-push`;
  - outbox backlog (`published_at IS NULL` older than 5 minutes);
  - API 5xx rate;
  - Cloud SQL CPU and storage.
- [ ] **Uptime check** on `https://asyncsvc.rasevicengineering.com/health`.
- [ ] **Purge old rows:** published `outbox` rows and old `email_deliveries` grow forever. Delete rows older than 30 days in the outbox sweep.
- [ ] **Database size and HA:** move off `db-f1-micro` (shared CPU, no SLA) to `db-custom-1-3840` or larger, and enable high availability for production.
- [ ] **Cold starts:** set `MinInstanceCount: 1` on the API in production for snappier first requests (about $10–15/month).
- [ ] **Long reports:** reports over ~9 minutes don't fit Cloud Run push delivery. Run them as Cloud Run Jobs, or use `platform=gke`.
- [ ] **Identity Platform perpetual diff:** every deploy shows `~ gcp:identityplatform:Config auth update [diff: ~signIn]`. Set the fields Google fills in by default explicitly, or use `ignoreChanges`, so previews stay clean.
- [ ] **Staging and production environments:** new Pulumi stacks in their own GCP projects, each with a GitHub environment and deployer (see "Adding staging or production later" in the README).
- [ ] **Queue-depth autoscaling on GKE** with KEDA (`gcp-pubsub` scaler on `reports-pull`).

## Features

- [ ] **SMS notifications** through Twilio Programmable Messaging. Needs A2P 10DLC or toll-free verification for US numbers (days to weeks), phone number and consent capture, and an SMS sender in the worker next to email.
- [ ] **Scheduled and recurring reports** (Cloud Scheduler → outbox), plus cancel and delete for reports.
- [ ] **More report types,** and custom date presets ("last 7 days", "month to date").
- [ ] **User management UI** for admins: list users, grant and revoke admin (wrapping `adminctl`), disable accounts.
- [ ] **Activity feed extras:** search by report ID or email, and a link from a dead-lettered message to the `dead-letter-inspect` subscription.

## Live sports data

- [ ] **Scale the ingester to zero when idle:** run it only around NHL game times (Cloud Scheduler toggling the worker pool's instance count), or as a scheduled Cloud Run Job, instead of 24/7 (~$50/month).
- [ ] **More sources** on the same pattern: MLB Stats API (pitch-by-pitch), NBA live data, Wikimedia EventStreams.
- [ ] **NHL play corrections:** the NHL sometimes edits plays after the fact (e.g. reassigning a goal). Only new plays are ingested today; re-check recent plays and publish corrections.
- [ ] **Replay any StatsBomb match**, not just the curated catalog, by browsing `competitions.json` from the UI.
- [ ] **Visualizations:** shot maps from the rink and pitch coordinates already in `properties`.

## Developer experience

- [ ] **Browser end-to-end tests** (Playwright) for sign-in, report request, downloads and the Activity feed, run in CI against the docker-compose stack.
- [ ] **Dependency updates** with Dependabot or Renovate (Go modules, npm, GitHub Actions, base images).
- [ ] **Local `/version`:** pass `GIT_SHA` as a build arg in docker-compose so local builds report a commit instead of `dev`.
