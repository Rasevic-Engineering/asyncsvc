# Turning the live sports ingester on and off

The ingester streams live play-by-play (NHL, MLB, NBA, NFL, soccer) and StatsBomb soccer replays into the events pipeline ([README › Live sports data](../README.md#live-sports-data)). It runs as one always-on instance: a Cloud Run worker pool `asyncsvc-ingester`, or the `ingester` Deployment on GKE. That costs roughly **$50/month**, so turn it off when you're not using it.

There are two switches:

| Setting | Default | Effect |
|---|---|---|
| `ingesterEnabled` | `true` | `false` scales the ingester to **zero instances**: no live polling, no replays, no cost |
| `ingesterLeagues` | `nhl,mlb,nba,nfl,soccer` | Which leagues to poll live. `none` keeps the ingester running for **soccer replays only**. The instance cost stays the same |

## Turn it off or on (recommended)

Change the setting in the stack config and deploy. Because `infra/Pulumi.dev.yaml` is committed, a change made this way **sticks**: every later deploy, from CI or your Mac, keeps it.

```bash
cd ~/rasevicengineering/asyncsvc/infra
pulumi config set ingesterEnabled false     # off
# pulumi config set ingesterEnabled true    # back on
```

Then deploy it, through CI or from your machine.

**Through CI** (the usual way):

```bash
cd ~/rasevicengineering/asyncsvc
git add infra/Pulumi.dev.yaml
git commit -m "Turn the ingester off"
git push
```

Then approve **Deploy to dev** in GitHub Actions.

**From your machine:**

```bash
cd ~/rasevicengineering/asyncsvc
export PULUMI_CONFIG_PASSPHRASE_FILE=~/.config/pulumi/asyncsvc-dev.passphrase
make deploy
```

If you deploy from your machine, still commit and push `Pulumi.dev.yaml` afterwards. Otherwise the next CI deploy switches the ingester back to whatever the committed file says.

`pulumi preview` should show a single change, `~ gcp:cloudrunv2:WorkerPool ingester update [diff: ~scaling]` (or `~ kubernetes:apps/v1:Deployment ingester` on GKE).

### Choosing leagues

To poll only some leagues, or none while keeping replays available:

```bash
cd ~/rasevicengineering/asyncsvc/infra
pulumi config set ingesterLeagues nhl,mlb                 # just these
pulumi config set ingesterLeagues none                    # replays only
# pulumi config set ingesterLeagues nhl,mlb,nba,nfl,soccer  # all (default)
```

Deploy it the same way as above.

## Quick, temporary toggle (no deploy)

To pause the ingester right away, for example to stop spending while you're away, scale the worker pool directly:

```bash
gcloud run worker-pools update asyncsvc-ingester --region us-central1 --project asyncsvc-dev --instances 0
```

Turn it back on:

```bash
gcloud run worker-pools update asyncsvc-ingester --region us-central1 --project asyncsvc-dev --instances 1
```

Change the leagues without a deploy:

```bash
gcloud run worker-pools update asyncsvc-ingester --region us-central1 --project asyncsvc-dev --update-env-vars INGEST_LEAGUES=none
```

**This doesn't stick.** Pulumi manages the worker pool, so the **next deploy resets it** to `ingesterEnabled` / `ingesterLeagues` from the stack config. Use the Pulumi setting for anything longer than a quick pause.

On GKE, the equivalent is:

```bash
kubectl -n asyncsvc scale deployment ingester --replicas 0
```

It's reset by the next deploy too.

## Check whether it's running

```bash
gcloud run worker-pools describe asyncsvc-ingester --region us-central1 --project asyncsvc-dev \
  --format="value(scaling.manualInstanceCount)"
```

`1` means on, `0` means off.

Recent logs:

```bash
gcloud logging read 'resource.type="cloud_run_worker_pool" AND resource.labels.worker_pool_name="asyncsvc-ingester"' \
  --project asyncsvc-dev --freshness=15m --limit 20 --format="value(timestamp,jsonPayload.message)"
```

In the app, the **Live data** page shows "No games live" with no card updates, and replays stay on "Waiting for the ingester…".

## What happens while it's off

| | Behavior |
|---|---|
| **Replays already running** | Paused, not lost. On shutdown each replay saves its position and stays `running`; when the ingester comes back it **resumes from where it stopped** |
| **New replay requests** | Accepted by the API and **queued** in the `ingest-commands-pull` subscription (kept up to 7 days). They start when the ingester is back on |
| **Stop requests** | Also queued; applied when it's back on |
| **Live games** | Not polled. When the ingester comes back **during** a game, it catches up on that game's plays since the last one ingested. Those plays get the time they were ingested, not when they happened. Games that **start and finish** while it's off are skipped |
| **Everything else** | Unaffected: API, worker, reports, emails, Activity feed, and `POST /v1/events` |

## Local development

The docker-compose stack runs the ingester too, polling the real APIs for every league.

```bash
cd ~/rasevicengineering/asyncsvc/app
docker compose stop ingester       # off
docker compose start ingester      # on
```

To change the leagues, edit `INGEST_LEAGUES` under `ingester` in `app/docker-compose.yml` (`none` = replays only), then:

```bash
docker compose up -d ingester
```
