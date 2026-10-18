.PHONY: image release state-bootstrap test test-integration web-install web-dev web-build dev dev-down deploy destroy

test:
	cd app && go vet ./... && go test ./...
	cd web && npm run typecheck

# Embedded Postgres + in-process Pub/Sub push simulation; no Docker needed.
test-integration:
	cd app && go test -tags integration ./internal/e2e/ -count=1

web-install:
	cd web && npm install

# Next.js dev server with hot reload on :3000, proxying /v1, /config.json and
# /files to the API on :8080 (start the backend with `make dev` first).
web-dev:
	cd web && npm run dev

web-build:
	cd web && npm run build

# Full local stack: Postgres, Pub/Sub + Firebase Auth emulators, API + web app (:8080), worker (:8081).
# WORKER_MODE=pull make dev runs the worker like it runs on GKE.
dev:
	cd app && docker compose up --build

dev-down:
	cd app && docker compose down -v

# One-time: create the GCS bucket for Pulumi state and log in to it.
#   make state-bootstrap PROJECT=my-project [REGION=us-central1]
state-bootstrap:
	@test -n "$(PROJECT)" || (echo "usage: make state-bootstrap PROJECT=<gcp-project-id> [REGION=...]" && exit 1)
	cd infra && ./bootstrap-state.sh $(PROJECT) $(or $(REGION),us-central1)

# Container image: built with buildx for Cloud Run's linux/amd64, pushed to
# the stack's Artifact Registry repo, and pinned by digest in the stack
# config (asyncsvc:image). Pulumi then deploys exactly that build.
STACK_PROJECT = $(shell cd infra && pulumi config get gcp:project 2>/dev/null)
STACK_REGION  = $(or $(shell cd infra && pulumi config get gcp:region 2>/dev/null),us-central1)
REGISTRY      = $(STACK_REGION)-docker.pkg.dev
IMAGE_REPO    = $(REGISTRY)/$(STACK_PROJECT)/asyncsvc/app
IMAGE_TAG    ?= $(shell date -u +%Y%m%d-%H%M%S)
IMAGE_META    = /tmp/asyncsvc-image-metadata.json

image:
	@test -n "$(STACK_PROJECT)" || (echo "Set gcp:project on the Pulumi stack first (cd infra && pulumi config set gcp:project ...)" && exit 1)
	gcloud auth configure-docker $(REGISTRY) --quiet
	docker buildx build --platform linux/amd64 \
		-t $(IMAGE_REPO):$(IMAGE_TAG) \
		--metadata-file $(IMAGE_META) --push .
	cd infra && pulumi config set image \
		"$(IMAGE_REPO)@$$(python3 -c 'import json; print(json.load(open("$(IMAGE_META)"))["containerimage.digest"])')"
	@echo "Pinned asyncsvc:image to the new build. Run 'make deploy' to roll it out."

deploy:
	cd infra && pulumi up

# Build, push and deploy in one go.
release: image deploy

destroy:
	cd infra && pulumi destroy
