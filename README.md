# asyncsvc

A Go REST service that keeps slow work off the request path, a Next.js frontend with Firebase sign-in, and a Pulumi program (also Go) that builds the GCP infrastructure.

- **Reports**: `POST /v1/reports` returns `202 Accepted` right away. A worker queries BigQuery, streams CSV to Cloud Storage, and emails a link.
- **Emails**: sent asynchronously from a queue, with retries, dedupe and dead-lettering.
- **Web UI** (Next.js App Router, static export): sign in, request reports, watch them move from queued to ready, and download CSVs. Admins can also see every user's reports and send test events. The Go API serves the exported files from the same origin, so there's no CORS setup, no Node runtime in production, and no separate frontend service.
- **Auth**: Firebase Authentication (Identity Platform). Email/password sign-in, plus Google when an OAuth client is configured. The API verifies the Firebase ID token on every `/v1` request.
- **Analytics events**: `POST /v1/events` publishes to Pub/Sub, and a **BigQuery subscription** writes the events into a partitioned table. No consumer code is involved.

## Live sports data

A separate **ingester** (`/bin/ingester`) streams real sports data into the same events pipeline, so reports and the Activity feed have live data to process:

| Source | What | How |
|---|---|---|
| **NHL** ([public API](https://api-web.nhle.com)) | Every play of every live game: shots with rink coordinates, goals, hits, faceoffs, penalties | Checks the schedule every minute and polls each live game every 10s. Only new plays are published; the position is kept in Firestore, so restarts don't duplicate |
| **StatsBomb** ([open data](https://github.com/statsbomb/open-data)) | Every on-ball event of a famous match (passes, carries, shots with expected goals, fouls), replayed in its original timing at 1×–60× | Admin starts a replay on the **Live data** page → `POST /v1/admin/replays` → topic `ingest-commands` → ingester. Up to 3 at once; replays **resume after a restart** |

```
NHL API ◀─poll─┐
               ingester ──▶ topic "events" ──▶ BigQuery analytics.events (source, game_id, team, properties)
admin UI ─▶ API ─▶ topic "ingest-commands" ─pull─┘   └─▶ Firestore "ingest" ──▶ Live data page (real time)
```

- **Event shape:**
  - `event_type`: `nhl.<play>` (e.g. `nhl.shot-on-goal`) or `soccer.<event>` (e.g. `soccer.pass`);
  - `user_id`: the player;
  - `team` and `game_id`;
  - `properties`: period, clock, coordinates, shot type, outcome, expected goals, …

  Event IDs are deterministic, so a duplicate is easy to spot.
- **Reports:** *Sports activity* (event counts per game, team and type) and *Shots by team* (shots, on target, goals, expected goals; shootouts excluded).
- **Runs as:** a **Cloud Run worker pool** (always on, no HTTP traffic), or a `Deployment` on GKE. One instance costs roughly **$50/month**. **To turn it off and on**, or to stop just NHL polling, see **[docs/ingester.md](docs/ingester.md)**.
- **Locally:** `make dev` runs the ingester too, polling the real NHL API (set `NHL_ENABLED=false` to stop that). Replays work from http://localhost:8080/live.
- The data is used under the NHL API's and StatsBomb's non-commercial terms. The Live data page credits both.

## Admin Activity feed

Admins get an **Activity** page (`/activity`) that shows Pub/Sub messages live, as they're published and processed. Each message appears with its topic, a summary, its current state, and a timeline of its stages, with timings and errors.

```
API, worker ──publish──▶ topic "activity" ──┬─▶ worker ──▶ Firestore "activity" ──▶ browser (real-time listener)
                                            └─▶ BigQuery analytics.message_activity (full history)
```

- **Stages:** `published` (outbox relay or events API), `received`, then `acked`, `retrying`, `nacked` or `dead_lettered` (worker), with attempt number, duration and error.
- **Best-effort recording:** events are published without waiting, so recording never slows or fails real work. Activity about activity is never recorded, which would loop.
- **Real time:** the page uses Firestore's listener (the Firebase SDK keeps the connection), so there's no polling and no long-lived connections on Cloud Run.
- **Access:** Firestore security rules ([`infra/firestore.rules.tmpl`](infra/firestore.rules.tmpl)) allow reads only for admins (the `admin` claim, or a verified email in `adminEmails`), and no browser writes. Rules can't check token revocation, so a revoked admin can still read this feed until their token expires (≤1h); the API cuts them off immediately.
- **Retention:** a Firestore TTL policy deletes events after 7 days. Query the full history in BigQuery, for example retry rates or p95 processing time per topic.
- **Locally:** the docker-compose Firebase emulator runs Firestore too, with rules from `app/firebase/firestore.rules`.

## Authentication and authorization

| Role | Who | Can |
|---|---|---|
| user | anyone signed in | create reports; list, view and download **their own** reports; get email notifications at **their own verified** address |
| admin | custom claim `admin: true`, or a verified email in `adminEmails` / `ADMIN_EMAILS` | everything above, plus see **all** reports (`?scope=all`) and publish analytics events |

- Requests without a valid token get `401`. A user asking for someone else's report gets `404` (not `403`), so report IDs can't be probed.
- The API checks the token's signature, expiry, issuer and audience (your project). It doesn't check revocation on every request; tokens expire after an hour.
- Downloads use short-lived signed URLs: GCS V4 signed URLs in the cloud, HMAC-signed `/files/...` links locally. The browser can download without sending the token.
- **Grant and revoke admin with `adminctl`** (sets the `admin` custom claim; uses your gcloud credentials):

  ```bash
  make admin-grant  EMAIL=someone@example.com   # applies when their sign-in refreshes (≤1h, or on next sign-in)
  make admin-revoke EMAIL=someone@example.com   # immediate: signs them out everywhere
  make admin-list                               # admins via claim and via adminEmails
  make admin-show   EMAIL=someone@example.com
  ```

  Revocation is immediate because the API re-checks tokens that carry the admin claim (one Firebase lookup, on admin requests only). Admins from `adminEmails` can't be revoked this way: remove them from that config and redeploy. Locally, run `adminctl` with `FIREBASE_AUTH_EMULATOR_HOST=localhost:9099 GCP_PROJECT=demo-asyncsvc`.
- The page's Content-Security-Policy allows only this origin plus Firebase's auth endpoints. Next's inline scripts are allowed by SHA-256 hash, computed when the API starts, not by `'unsafe-inline'`.

## Platforms

Set `asyncsvc:platform` to choose where the service runs. The data plane is the same on both: Cloud SQL, Pub/Sub, BigQuery, Cloud Storage and Secret Manager.

| | `cloudrun` (default) | `gke` (GKE Autopilot) |
|---|---|---|
| Worker gets messages by | **Push**: Pub/Sub POSTs to the worker with an OIDC token | **Streaming pull**: no public endpoint needed |
| Max report runtime | about 9 minutes (Pub/Sub's 600s push ack deadline) | 25 minutes; the client library keeps extending the ack deadline |
| Outbox sweeper | Cloud Scheduler calls `/tasks/relay-outbox` every minute | Runs inside the worker every 30s; safe across replicas |
| Database connection | Cloud Run's built-in Cloud SQL socket | Cloud SQL Go connector |
| Workload identity | A Cloud Run service account per service | Workload Identity: Kubernetes SA → Google SA, no keys |
| API exposure | Cloud Run URL | LoadBalancer Service (internal unless `publicApi=true`), HPA, PDB |
| Scaling | Automatic, down to zero | API: HPA on CPU. Worker: fixed replicas (use KEDA for queue-based scaling) |
| Idle cost | Close to zero apart from Cloud SQL | Autopilot bills pod requests, so expect a few dollars a day for two API pods and two workers |

The same image runs on both. `WORKER_MODE=push|pull` switches how the worker receives messages; the handlers, retries and dedupe are identical.

## Architecture (Cloud Run shown)

```
            ┌──────────────── Cloud Run: api ────────────────┐
 client ──▶ │ POST /v1/reports  → tx{ reports row + outbox } │──┐ publish after commit
            │ GET  /v1/reports/{id} → status + signed URL    │  │
            │ POST /v1/events   → publish                    │  │
            └────────────────────────────────────────────────┘  │
                         │ Cloud SQL (Postgres)                 ▼
                         ▼                            Pub/Sub topics
            reports · outbox · email_deliveries     ┌─────────────────┐
                         ▲                          │ report-requests │──push (OIDC)──┐
                         │                          │ email-requests  │──push (OIDC)──┤
            ┌──────── Cloud Run: worker ───────┐    │ events          │──BigQuery sub─┼──▶ BigQuery analytics.events
            │ /pubsub/reports  ◀───────────────┼────┤ dead-letter     │◀── after N failed attempts
            │ /pubsub/emails   ◀───────────────┼────┘ └───────────────┘               │
            │ /tasks/relay-outbox ◀── Cloud Scheduler (every minute)                  │
            └──────────────────────────────────┘                                     │
                │ BigQuery (Storage Read API) ──▶ CSV ──▶ Cloud Storage (30-day lifecycle)
                └─▶ SendGrid (or log mailer)
```

| GCP service | Role |
|---|---|
| Cloud Run | `api` (public or IAM-protected) and `worker` (accepts only authenticated requests from Pub/Sub and Scheduler) |
| Cloud SQL Postgres 16 | Report state, transactional outbox, email dedupe |
| Pub/Sub | Work queues with exponential-backoff retries and a dead-letter topic; BigQuery subscription for events |
| BigQuery | `analytics.events`, partitioned by day and clustered by `event_type`; the report source |
| Cloud Storage | Generated reports, downloaded through V4 signed URLs |
| Cloud Scheduler | Outbox sweeper, every minute |
| Secret Manager | DB password, SendGrid key |
| Artifact Registry | Container image, built and pushed by Pulumi |

## How the async guarantees work

- **Transactional outbox.** The report row and its queue message are committed in one Postgres transaction, so you can never have a report with no job or a job with no report. The API publishes right after commit (fast path). If Pub/Sub is down, the request still succeeds, and the Scheduler sweep publishes the message within a minute.
- **At-least-once delivery means idempotent handlers.**
  - Reports are *claimed* with a conditional `UPDATE`. A duplicate delivery of a finished report is acked. A duplicate delivery of a report that is still running is nacked and retried later. A claim older than 15 minutes is treated as a crashed run and can be reclaimed.
  - Emails are deduped on the outbox id in `email_deliveries`.
- **Retries and dead-lettering.**
  - Transient errors return `503`, so Pub/Sub retries with a 10s → 600s backoff.
  - Permanent errors (an invalid request, a bad template) are acked so they don't loop.
  - On the final attempt, the report is marked `failed`, a failure email is queued, and the message goes to the `dead-letter` topic. You can inspect it with `gcloud pubsub subscriptions pull dead-letter-inspect`.
- **Long jobs.** The reports subscription uses Pub/Sub's maximum 600s ack deadline. Generation has a 9-minute timeout, and the worker's Cloud Run request timeout is 900s. Reports that need longer should move to Cloud Run Jobs (see Next steps).
- **Big results.** BigQuery rows stream through the Storage Read API → `encoding/csv` → the GCS writer, so memory stays flat. A failed run aborts the upload, so no partial file is left behind.

## Repository layout

```
Dockerfile               one image: Next.js static export + Go binaries (/bin/api, /bin/worker, /bin/devsetup)
web/                     Next.js app (App Router, output: "export")
  src/app/               routes: / (dashboard), /login
  src/lib/auth.tsx       Firebase auth context (sign in/up, Google, sign out)
  src/lib/api.ts         typed API client; attaches the ID token
  src/components/        ReportForm, ReportList, EventsPanel, Header, RequireAuth, ...
app/                     Go service
  cmd/api, cmd/worker    entrypoints
  cmd/devsetup           creates topics/push subs on the Pub/Sub emulator
  internal/api           REST handlers + static serving of the web app (spa.go)
  internal/auth          Firebase ID token verification + role checks
  internal/worker        Pub/Sub push + scheduler handlers
  internal/outbox        transactional outbox + relay
  internal/reports       model, repo, BigQuery + fake sources
  internal/blob          GCS / local file storage
  internal/mail          templates, SendGrid, log mailer
  internal/db            pool + embedded SQL migrations
  internal/e2e           integration tests (embedded Postgres)
  docker-compose.yml     local stack
infra/                   Pulumi (Go)
  main.go                settings + shared data plane (SQL, Pub/Sub, BigQuery, GCS, IAM, image)
  cloudrun.go            platform=cloudrun: services, push subscriptions, Scheduler
  gke.go                 platform=gke: Autopilot cluster, Workload Identity, Deployments, pull subscriptions
Makefile
```

## Run locally

Requires a running Docker daemon with the Compose plugin.

```bash
make dev
```

That runs push mode, like Cloud Run. To run the worker in pull mode, like GKE:

```bash
make dev-down && WORKER_MODE=pull make dev
```

This starts Postgres, the Pub/Sub emulator, the **Firebase Auth emulator**, the API with the web UI on **http://localhost:8080**, and the worker on `:8081`.

Locally, sign up with any email and password; nothing leaves your machine. `admin@example.com` gets the admin role. The emulator also fakes "Continue with Google". Email verification is skipped locally (`AUTH_TRUST_UNVERIFIED_EMAIL`), and the API refuses that setting unless it's talking to the emulator. Locally, reports come from a fake source (BigQuery has no emulator), files go to a Docker volume, and emails are written to the worker log.

```bash
curl -si localhost:8080/v1/reports -d '{"kind":"daily_active_users","from":"2026-01-01","to":"2026-01-08","notify_email":"me@example.com"}'
curl -s localhost:8080/v1/reports/<id>
curl -si localhost:8080/v1/events -d '[{"event_type":"signup","user_id":"u1","properties":{"plan":"pro"}}]'
```

### Frontend development

`make dev` serves the built UI from the image. For hot reload while working on the frontend, leave the stack running and also start:

```bash
make web-install
make web-dev
```

Open http://localhost:3000. `next dev` proxies `/v1`, `/config.json` and `/files` to the API on `:8080`.

### API

| Method | Path | |
|---|---|---|
All `/v1` routes need `Authorization: Bearer <Firebase ID token>`.

| Method | Path | Who | |
|---|---|---|---|
| `GET` | `/v1/me` | user | Caller's uid, email, verification and admin status |
| `POST` | `/v1/reports` | user | `{kind, from, to, notify}`; returns `202` + `Location` |
| `GET` | `/v1/reports?limit=50&scope=mine` | user | Caller's reports, newest first; `scope=all` is admin-only |
| `GET` | `/v1/reports/{id}` | owner or admin | Status, plus a 5-minute signed `download_url` when ready |
| `POST` | `/v1/events` | admin | Publish 1–500 analytics events |
| `GET` | `/config.json` | public | Firebase web config for the browser |
| `GET` | `/healthz` | public | Liveness |
| `GET` | `/*` | public | The web app (`/login` → `login.html`; unknown paths get the 404 page) |

### Tests

```bash
make test-integration
```

This needs no Docker. It runs the real API and worker handlers against an embedded Postgres and simulates Pub/Sub push. It covers:
- the happy path
- duplicate deliveries
- email dedupe
- retry-then-succeed
- final-attempt failure and the dead-letter path
- outbox recovery when Pub/Sub is down
- request validation
- the list endpoint and signed download links (including tampered links)
- authorization: anonymous and forged tokens, ownership, admin-only scope and events, notification address
- web app serving: Next.js route resolution, 404 page, cache headers, CSP script hashes, path traversal

## Deploy to GCP

Prerequisites: the `pulumi` CLI, the `gcloud` CLI, a running Docker daemon, and a GCP project with billing enabled.

```bash
gcloud auth login
gcloud auth application-default login
gcloud services enable cloudresourcemanager.googleapis.com serviceusage.googleapis.com --project my-gcp-project

cd infra
make state-bootstrap PROJECT=my-gcp-project   # one-time: GCS bucket for Pulumi state + pulumi login
pulumi stack init dev
pulumi config set gcp:project my-gcp-project
pulumi config set gcp:region us-central1
pulumi config set adminEmails you@yourdomain.com   # admin role once that email is verified
pulumi config set deletionProtection false    # dev only
pulumi config set --secret sendgridApiKey SG.xxx   # optional; otherwise emails are logged
export PULUMI_CONFIG_PASSPHRASE_FILE=~/.config/pulumi/asyncsvc-dev.passphrase   # or PULUMI_CONFIG_PASSPHRASE
ASYNCSVC_IMAGE=skip pulumi up   # first run only: data plane + registry, no API/worker yet
cd .. && make release           # build + push the image, deploy the API and worker with its digest
```

The first deploy takes about 20 minutes, mostly creating the Cloud SQL instance. After that, `make release` is the everyday command: it builds the image with `docker buildx` for `linux/amd64`, pushes it to Artifact Registry, and runs `pulumi up` with that digest in `ASYNCSVC_IMAGE`. `make deploy` applies infrastructure changes and keeps the image that's live (read from the stack's `image` output in shared state), so a local deploy can never roll back what CI shipped. To roll back, run `cd infra && ASYNCSVC_IMAGE=<repo>@sha256:<older digest> pulumi up`. A missing image is an error, never "delete the services".

### Inspecting the database

See **[docs/cloud-sql.md](docs/cloud-sql.md)**. It covers connecting to Cloud SQL from your machine through the Cloud SQL Auth Proxy, loading the database password from Secret Manager into `PGPASSWORD`, read-only sessions, and sample queries for reports, the outbox and emails.

### CI/CD (GitHub Actions)

[`.github/workflows/ci.yml`](.github/workflows/ci.yml) runs on every pull request and push:

| Job | When | What |
|---|---|---|
| Go (API, worker, infra) | always | `go vet`, unit tests, integration tests (embedded Postgres), infra compiles |
| Next.js web app | always | `npm ci`, type-check, static export build |
| Deploy to dev | push to `main`, after both pass | **waits for approval**, then builds the image (native amd64, layer cache), `pulumi up` with that digest, then smoke-tests `/health` and `/v1/me` |

GitHub authenticates to GCP with **Workload Identity Federation**, so no service account key exists anywhere. GCP trusts only runs that are all of: this repository, the `main` branch, and the `dev` GitHub environment. The trust rule and the `asyncsvc-deployer` service account are in [`infra/cicd.go`](infra/cicd.go), configured per stack:

```bash
pulumi config set githubRepository Rasevic-Engineering/asyncsvc
pulumi config set githubEnvironment dev     # defaults to the stack name
pulumi config set githubBranch main
```

One-time GitHub setup (Settings > Environments):

1. Create the environment **`dev`**: add yourself under **Required reviewers**, and limit **Deployment branches** to `main`.
2. Add the environment secret **`PULUMI_CONFIG_PASSPHRASE`** (the stack passphrase).

**Adding staging or production later:**
1. Create a stack (`pulumi stack init prod`) pointing at its own GCP project, with `githubEnvironment` and `githubBranch` set.
2. Run `make state-bootstrap`, then `cd infra && ASYNCSVC_IMAGE=skip pulumi up` once locally to create its deployer.
3. Copy the `deploy-dev` job with the new environment's values.

Each environment gets its own deployer identity, so one can never deploy another.

### GKE instead of Cloud Run

```bash
gcloud components install gke-gcloud-auth-plugin kubectl
pulumi config set platform gke
pulumi up
$(pulumi stack output getCredentials)     # configures kubectl
kubectl -n asyncsvc get pods,svc,hpa
kubectl -n asyncsvc logs deploy/worker -f
```

Creating the Autopilot cluster adds about 10 minutes to the first `pulumi up`. With `publicApi=false`, the API gets an internal load balancer, reachable only from inside the VPC. Use `kubectl -n asyncsvc port-forward svc/api 8080:80` to test it from your machine.

Switching `platform` on an existing stack replaces the compute layer: Cloud Run and its push subscriptions are removed, and the cluster and pull subscriptions are created. Data in Cloud SQL, BigQuery and GCS is kept.

Then try it:

```bash
API=$(pulumi stack output apiUrl)
AUTH="Authorization: Bearer $(gcloud auth print-identity-token)"   # only needed if publicApi=false
curl -si -H "$AUTH" $API/v1/events -d '[{"event_type":"signup","user_id":"u1"},{"event_type":"login","user_id":"u1"}]'
# Events reach BigQuery within seconds:
bq query --nouse_legacy_sql 'SELECT * FROM analytics.events ORDER BY received_at DESC LIMIT 10'
curl -si -H "$AUTH" $API/v1/reports -d '{"kind":"events_by_type","from":"2026-10-01","to":"2026-10-12"}'
```

### Configuration (`asyncsvc:*`)

| Key | Default | |
|---|---|---|
| `dbTier` | `db-f1-micro` | Use `db-custom-1-3840` or larger for production |
| `bigqueryLocation` | `US` | |
| `mailFrom` | `reports@example.com` | Must be a verified sender in SendGrid |
| `sendgridApiKey` (secret) | unset | Unset means `MAIL_PROVIDER=log` |
| `platform` | `cloudrun` | `cloudrun` or `gke` |
| `workerReplicas` | `2` | GKE only |
| `publicApi` | `true` | Network-level access to the API/UI. The API enforces Firebase auth either way. `false` = IAM-only on Cloud Run, internal load balancer on GKE |
| `customDomain` | empty | Cloud Run only: serve the app at this hostname through a global HTTPS load balancer with a Google-managed certificate and HTTP→HTTPS redirect (about $18/month). After deploying, create the DNS **A** record from the `customDomainDns` output; the certificate is issued once DNS resolves (15 minutes to a few hours)
| `ingesterEnabled` | `true` | Run the live sports ingester (1 always-on instance, ~$50/month); `false` scales it to zero. See [docs/ingester.md](docs/ingester.md) |
| `ingesterNhl` | `true` | `false` stops NHL polling and keeps soccer replays |
| `githubRepository` / `githubEnvironment` / `githubBranch` | empty / stack name / `main` | Which GitHub Actions runs may deploy this stack (see CI/CD) |
| `adminEmails` | empty | Comma-separated; these verified emails get the admin role |
| `googleOAuthClientId` / `googleOAuthClientSecret` (secret) | unset | Enables Google sign-in. Create a "Web application" OAuth client in the console first, with redirect URI `https://<project>.firebaseapp.com/__/auth/handler` |
| `deletionProtection` | `true` | Applies to Cloud SQL, the BigQuery table and the bucket |

## Next steps for production

The full backlog, including a step-by-step plan for **IAM database authentication** (passwordless, per-person, read-only-by-default access to Cloud SQL), is in **[docs/TODO.md](docs/TODO.md)**. Highlights:

- **Custom domain.** Put the API behind your own domain (a Cloud Run domain mapping or load balancer, or a GKE Gateway with a managed certificate) and add it to the authorized domains in `infra/auth.go`. On GKE this is required for Google sign-in.
- **Email verification UX.** Firebase sends its default verification email. Customize the template, or the sender domain, in the Firebase console. Options are Identity Platform/Firebase JWT middleware, API Gateway, or IAP.
- **Long reports.** On Cloud Run, have the worker trigger a **Cloud Run Job** for reports over ~9 minutes, or use `platform=gke`.
- **TLS on GKE.** Replace the LoadBalancer Service with a Gateway or Ingress plus a Google-managed certificate.
- **Queue-based worker scaling on GKE.** Install KEDA with a `gcp-pubsub` scaler on `reports-pull`.
- **Private networking.** Move Cloud SQL to private IP with Direct VPC egress.
- **CI/CD.** Run the `make image` steps in Cloud Build or GitHub Actions (build, push, `pulumi config set image <digest>`, `pulumi up`), authenticating with Workload Identity Federation rather than a key.
- **Alerting.** Alert on dead-letter topic message count, the oldest unacked message age on `reports-push`, and the outbox backlog (`SELECT count(*) FROM outbox WHERE published_at IS NULL`).
