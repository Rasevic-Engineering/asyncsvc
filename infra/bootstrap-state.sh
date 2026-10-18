#!/usr/bin/env bash
# One-time setup: creates the GCS bucket that stores Pulumi state for this
# project, then logs the Pulumi CLI into it.
#
#   ./bootstrap-state.sh PROJECT_ID [REGION]
#
# The bucket is deliberately NOT managed by the Pulumi program itself: it has
# to exist before Pulumi can store anything, and a `pulumi destroy` must never
# delete the state that describes what to destroy.
#
# Requires: gcloud auth login, gcloud auth application-default login
# (Pulumi reads and writes state with your application-default credentials).
set -euo pipefail

PROJECT="${1:?usage: $0 PROJECT_ID [REGION]}"
REGION="${2:-us-central1}"
BUCKET="${PROJECT}-pulumi-state"

gcloud services enable storage.googleapis.com --project "$PROJECT"

if gcloud storage buckets describe "gs://$BUCKET" --project "$PROJECT" >/dev/null 2>&1; then
  echo "Bucket gs://$BUCKET already exists; reusing it."
else
  gcloud storage buckets create "gs://$BUCKET" \
    --project "$PROJECT" \
    --location "$REGION" \
    --uniform-bucket-level-access \
    --public-access-prevention
fi

# Versioning keeps every previous state file, so a bad update or an
# accidental delete can be rolled back. Old versions expire after 90 days.
gcloud storage buckets update "gs://$BUCKET" --versioning

lifecycle="$(mktemp)"
trap 'rm -f "$lifecycle"' EXIT
cat >"$lifecycle" <<'JSON'
{
  "rule": [
    {
      "action": { "type": "Delete" },
      "condition": { "isLive": false, "daysSinceNoncurrentTime": 90 }
    }
  ]
}
JSON
gcloud storage buckets update "gs://$BUCKET" --lifecycle-file="$lifecycle"

pulumi login "gs://$BUCKET"

cat <<EOF

Pulumi state backend: gs://$BUCKET

Next:
  cd infra
  pulumi stack init dev        # asks for a passphrase that encrypts secrets in state
  pulumi config set gcp:project $PROJECT
  pulumi config set gcp:region $REGION

Keep the passphrase somewhere safe (a password manager). Without it Pulumi
can't decrypt the stack's secrets. Export it as PULUMI_CONFIG_PASSPHRASE to
avoid typing it on every command.
EOF
