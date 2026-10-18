.PHONY: admin-grant admin-revoke admin-list admin-show image release state-bootstrap test test-integration web-install web-dev web-build dev dev-down deploy destroy

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

# Admin role management (Firebase custom claim). Uses your gcloud
# application-default credentials and the stack's project and adminEmails.
#   make admin-grant EMAIL=someone@example.com
#   make admin-revoke EMAIL=someone@example.com
#   make admin-list
#   make admin-show EMAIL=someone@example.com
ADMINCTL = cd app && GCP_PROJECT=$(STACK_PROJECT) \
	ADMIN_EMAILS="$$(cd ../infra && pulumi config get adminEmails 2>/dev/null)" \
	go run ./cmd/adminctl

admin-grant:
	@test -n "$(EMAIL)" || (echo "usage: make admin-grant EMAIL=someone@example.com" && exit 1)
	$(ADMINCTL) grant $(EMAIL)

admin-revoke:
	@test -n "$(EMAIL)" || (echo "usage: make admin-revoke EMAIL=someone@example.com" && exit 1)
	$(ADMINCTL) revoke $(EMAIL)

admin-list:
	$(ADMINCTL) list

admin-show:
	@test -n "$(EMAIL)" || (echo "usage: make admin-show EMAIL=someone@example.com" && exit 1)
	$(ADMINCTL) show $(EMAIL)

# One-time: create the GCS bucket for Pulumi state and log in to it.
#   make state-bootstrap PROJECT=my-project [REGION=us-central1]
state-bootstrap:
	@test -n "$(PROJECT)" || (echo "usage: make state-bootstrap PROJECT=<gcp-project-id> [REGION=...]" && exit 1)
	cd infra && ./bootstrap-state.sh $(PROJECT) $(or $(REGION),us-central1)

# Container image: built with buildx for Cloud Run's linux/amd64 and pushed to
# the stack's Artifact Registry repo. Deploys pass it to Pulumi by digest in
# ASYNCSVC_IMAGE; the deployed digest is kept in the stack's `image` output
# (shared state), never in the committed stack file.
STACK_PROJECT = $(shell cd infra && pulumi config get gcp:project 2>/dev/null)
STACK_REGION  = $(or $(shell cd infra && pulumi config get gcp:region 2>/dev/null),us-central1)
REGISTRY      = $(STACK_REGION)-docker.pkg.dev
IMAGE_REPO    = $(REGISTRY)/$(STACK_PROJECT)/asyncsvc/app
GIT_SHA       = $(shell git rev-parse HEAD 2>/dev/null || echo unknown)$(shell git diff --quiet HEAD 2>/dev/null || echo -dirty)
IMAGE_TAG    ?= $(shell date -u +%Y%m%d-%H%M%S)
IMAGE_META    = /tmp/asyncsvc-image-metadata.json
IMAGE_DIGEST  = python3 -c 'import json; print(json.load(open("$(IMAGE_META)"))["containerimage.digest"])'

image:
	@test -n "$(STACK_PROJECT)" || (echo "Set gcp:project on the Pulumi stack first (cd infra && pulumi config set gcp:project ...)" && exit 1)
	gcloud auth configure-docker $(REGISTRY) --quiet
	docker buildx build --platform linux/amd64 --provenance=false \
		--build-arg GIT_SHA=$(GIT_SHA) \
		-t $(IMAGE_REPO):$(IMAGE_TAG) \
		--metadata-file $(IMAGE_META) --push .
	@echo "Pushed $(IMAGE_REPO)@$$($(IMAGE_DIGEST))"

# Build, push and deploy a new image.
release: image
	cd infra && ASYNCSVC_IMAGE="$(IMAGE_REPO)@$$($(IMAGE_DIGEST))" pulumi up

# Apply infrastructure changes, keeping the image that's live now.
deploy:
	cd infra && img="$$(pulumi stack output image)" && test -n "$$img" \
		|| { echo "No deployed image found; run 'make release' instead."; exit 1; }; \
		ASYNCSVC_IMAGE="$$img" pulumi up

destroy:
	cd infra && ASYNCSVC_IMAGE=skip pulumi destroy
