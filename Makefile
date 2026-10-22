SHELL := /usr/bin/env bash

GO := go
# buf on PATH by default (CI pins 1.50.0 in .github/workflows/ci.yaml); without it: make generate BUF='go run github.com/bufbuild/buf/cmd/buf@v1.50.0'
BUF ?= buf
BIN_DIR := bin
BACKEND_BIN := $(BIN_DIR)/ai-grid-provider-backend
FAKE_INFRA_BIN := $(BIN_DIR)/ai-grid-fake-infra

# Dev mode with fake infrastructure (make dev-fake). Override any of these,
# for example `make dev-fake DEV_GRPC_PORT=19090`. gRPC 9090 is what the
# console BFF dials by default; 9091 is where it sends SelfSubjectAccessReview;
# 9094 serves the console's draft API (ai_grid.draft.v1, docs/draft/README.md).
DEV_HOST ?= 127.0.0.1
DEV_GRPC_PORT ?= 9090
DEV_HUB_PORT ?= 9091
DEV_HTTP_PORT ?= 9092
DEV_KEYCLOAK_PORT ?= 9093
DEV_DRAFT_PORT ?= 9094
# make dev-fake DEV_DRAFT_ADDRESS= runs without the draft fake.
DEV_DRAFT_ADDRESS ?= $(DEV_HOST):$(DEV_DRAFT_PORT)
DEV_FIXTURES ?= fixtures/default
DEV_DRAFT_FIXTURES ?= fixtures/draft/default
DEV_KUBECONFIG ?= $(BIN_DIR)/fake-kubeconfig

# Container image (Podman preferred). Override with IMAGE=... TAG=dev.
# Default linux/amd64 so OpenShift nodes can run images built on Apple Silicon.
IMAGE ?= ghcr.io/ai-grid-cloud/ai-grid-provider-backend
TAG ?= latest
PLATFORM ?= linux/amd64
CONTAINER_ENGINE ?= $(shell command -v podman 2>/dev/null || command -v docker)

# Local API (make run). Uses your kubeconfig against the current oc/kubectl context.
LISTEN_ADDRESS ?= :8080
HTTP_LISTEN_ADDRESS ?= :8081
KUBECONFIG_PATH ?= $(if $(KUBECONFIG),$(KUBECONFIG),$(HOME)/.kube/config)
# Local Keycloak from `make keycloak-up` (port 8180 avoids clashing with gRPC/REST).
KEYCLOAK_URL ?= http://127.0.0.1:8180
KEYCLOAK_PORT ?= 8180

# Smoke tests hit a running API (make run). Override if needed.
GRPC_ADDR ?= localhost:8080
HTTP_ADDR ?= http://localhost:8081

.PHONY: help all binary build-bin build test unit smoke smoke-openshift vet fmt-check lint verify generate check-generated \
	image container push build-and-push run \
	keycloak-up keycloak-down keycloak-seed \
	build-fake-infra dev-fake dev-fake-poc \
	prereqs kind-test clean-kind openshift-up openshift-down acm-hub-up acm-hub-down

help: ## Show available targets
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } \
		/^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

all: verify

##@ Local binary

build: ## Build the Go binary into bin/
	mkdir -p $(BIN_DIR)
	$(GO) build -buildvcs=false -trimpath -o $(BACKEND_BIN) ./cmd/ai-grid-provider-backend

binary: build ## Alias for build
build-bin: build ## Deprecated alias for build

# Unit packages only (skips ./integration, which needs Kind via make kind-test).
UNIT_PKGS := $(shell $(GO) list ./... | grep -v '/integration$$')

unit: ## Run unit tests (excludes Kind integration)
	$(GO) test $(UNIT_PKGS)

test: unit ## Alias for unit

build-fake-infra: ## Build the fake hub, Keycloak and draft API (cmd/ai-grid-fake-infra)
	mkdir -p $(BIN_DIR)
	$(GO) build -buildvcs=false -trimpath -o $(FAKE_INFRA_BIN) ./cmd/ai-grid-fake-infra

# Runs the fake hub API, fake Keycloak and draft fake (cmd/ai-grid-fake-infra)
# and the real backend against them in one terminal; Ctrl-C stops both. See README.md,
# "Dev mode with fake infrastructure".
dev-fake: build build-fake-infra ## Run the real backend on a fake hub and Keycloak, plus the draft fake
	@DEV_HOST="$(DEV_HOST)" DEV_GRPC_PORT="$(DEV_GRPC_PORT)" DEV_HUB_PORT="$(DEV_HUB_PORT)" \
		DEV_HTTP_PORT="$(DEV_HTTP_PORT)" DEV_KEYCLOAK_PORT="$(DEV_KEYCLOAK_PORT)" \
		DEV_DRAFT_PORT="$(DEV_DRAFT_PORT)" DEV_DRAFT_ADDRESS="$(DEV_DRAFT_ADDRESS)" DEV_DRAFT_FIXTURES="$(DEV_DRAFT_FIXTURES)" \
		DEV_FIXTURES="$(DEV_FIXTURES)" DEV_KUBECONFIG="$(DEV_KUBECONFIG)" \
		FAKE_INFRA_BIN="$(FAKE_INFRA_BIN)" BACKEND_BIN="$(BACKEND_BIN)" \
		bash hack/dev-fake.sh

# The same on the PoC preset (fixtures/poc, fixtures/draft/poc).
dev-fake-poc: ## dev-fake on the PoC preset (fixtures/poc, fixtures/draft/poc)
	@$(MAKE) --no-print-directory dev-fake DEV_FIXTURES=fixtures/poc DEV_DRAFT_FIXTURES=fixtures/draft/poc

vet:
	$(GO) vet ./...

fmt-check:
	@files="$$(gofmt -l client cmd gen hack integration internal)"; \
	if [[ -n "$$files" ]]; then \
		echo "gofmt is required for:"; echo "$$files"; exit 1; \
	fi

lint: fmt-check vet ## gofmt check, go vet; golangci-lint when installed
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not installed; gofmt and go vet completed"; \
	fi

# Regenerates protobuf, gRPC, Connect, and OpenAPI (OSAC-style buf pipeline) for ai_grid.v1,
# then Go and gRPC only for the console's draft, ai_grid.draft.v1 (proto/buf.gen.draft.yaml,
# docs/draft/proto/README.md).
generate: ## Regenerate protobuf, RPC stubs, OpenAPI, and the draft's Go code
	cd proto && $(BUF) dep update && $(BUF) generate && $(BUF) generate --template buf.gen.draft.yaml
	# Drop legacy monolithic stubs if a prior generate left them behind.
	rm -f gen/ai_grid/v1/ai_grid.pb.go gen/ai_grid/v1/ai_grid_grpc.pb.go gen/ai_grid/v1/ai_grid.pb.gw.go
	mkdir -p internal/restgateway/openapi
	cp docs/openapi/ai_grid.swagger.json internal/restgateway/openapi/ai_grid.swagger.json

# Generates into a temporary directory and compares. This catches a file the
# protobuf no longer produces, which `git status` cannot: an orphaned generated
# file is committed and unmodified, so the worktree looks clean.
check-generated: ## fail when the committed output differs from what proto/ generates: stale, missing or left-over files
	tmp="$$(mktemp -d)" && trap 'rm -rf "$$tmp"' EXIT && \
	sed -e "s#out: \.\./gen\$$#out: $$tmp/gen#" \
	    -e "s#out: \.\./docs/openapi\$$#out: $$tmp/openapi#" \
	    proto/buf.gen.yaml > "$$tmp/buf.gen.yaml" && \
	if grep -q "out: \.\./" "$$tmp/buf.gen.yaml"; then \
	  echo "check-generated: an out: path in proto/buf.gen.yaml was not redirected; the check would compare the real output with itself" >&2; \
	  exit 1; \
	fi && \
	(cd proto && $(BUF) generate --template "$$tmp/buf.gen.yaml") && \
	diff -r "$$tmp/gen/ai_grid/v1" gen/ai_grid/v1 && \
	diff -r "$$tmp/openapi" docs/openapi && \
	diff "$$tmp/openapi/ai_grid.swagger.json" internal/restgateway/openapi/ai_grid.swagger.json && \
	sed -e "s#out: \.\./gen\$$#out: $$tmp/gen#" proto/buf.gen.draft.yaml > "$$tmp/buf.gen.draft.yaml" && \
	if grep -q "out: \.\./" "$$tmp/buf.gen.draft.yaml"; then \
	  echo "check-generated: an out: path in proto/buf.gen.draft.yaml was not redirected" >&2; \
	  exit 1; \
	fi && \
	(cd proto && $(BUF) generate --template "$$tmp/buf.gen.draft.yaml") && \
	diff -r "$$tmp/gen/ai_grid/draft/v1" gen/ai_grid/draft/v1

verify: build unit lint ## Build binary + unit tests + lint

##@ Container image (ghcr.io)

# Build the container image with Podman (falls back to docker if podman is missing).
# PLATFORM is os/arch (default linux/amd64). TARGETARCH is passed explicitly because
# Podman on Apple Silicon often leaves TARGETARCH unset, producing Exec format error on OpenShift.
image: ## Build the container image ($(IMAGE):$(TAG))
	@if [[ -z "$(CONTAINER_ENGINE)" ]]; then echo "podman or docker is required" >&2; exit 1; fi
	@target_arch="$$(echo "$(PLATFORM)" | awk -F/ '{print $$2}')"; \
	target_os="$$(echo "$(PLATFORM)" | awk -F/ '{print $$1}')"; \
	echo "building $(IMAGE):$(TAG) for $${target_os}/$${target_arch}"; \
	$(CONTAINER_ENGINE) build \
		--platform=$(PLATFORM) \
		--build-arg TARGETOS=$${target_os} \
		--build-arg TARGETARCH=$${target_arch} \
		-t $(IMAGE):$(TAG) \
		-f Containerfile .

container: image ## Alias for image

push: ## Push $(IMAGE):$(TAG) (podman login ghcr.io first)
	@if [[ -z "$(CONTAINER_ENGINE)" ]]; then echo "podman or docker is required" >&2; exit 1; fi
	@echo "pushing $(IMAGE):$(TAG)"
	$(CONTAINER_ENGINE) push $(IMAGE):$(TAG)

build-and-push: image push ## Build the image, then push it

##@ Local API

run: ## Run the API locally (gRPC :8080, REST/Swagger :8081)
	@echo "gRPC      $(LISTEN_ADDRESS)"
	@echo "HTTP      $(HTTP_LISTEN_ADDRESS)  (Swagger UI at /docs/)"
	@echo "kube      $(KUBECONFIG_PATH)"
	@echo "keycloak  $(KEYCLOAK_URL)"
	KEYCLOAK_URL=$(KEYCLOAK_URL) \
	$(GO) run ./cmd/ai-grid-provider-backend \
		--listen-address=$(LISTEN_ADDRESS) \
		--http-listen-address=$(HTTP_LISTEN_ADDRESS) \
		--kubeconfig=$(KUBECONFIG_PATH)

# Requires a running API (make run) plus hub kubeconfig + Keycloak for full coverage.
# SKIP_CLUSTERS=1 / SKIP_TENANTS=1 / SKIP_SUBSCRIPTIONS=1 to exercise a subset.
smoke: ## Live gRPC/REST smoke against localhost:8080/:8081
	GRPC_ADDR=$(GRPC_ADDR) HTTP_ADDR=$(HTTP_ADDR) bash hack/smoke-apis.sh
	@if [[ "$(SKIP_CLUSTERS)" != "1" ]]; then \
		GRPC_ADDR=$(GRPC_ADDR) bash hack/smoke-managedcluster-crud.sh; \
	else \
		echo "skipping ManagedCluster CRUD (SKIP_CLUSTERS=1)"; \
	fi

# Same smoke suite against the current oc context (port-forwards svc in NAMESPACE).
# Deploy first: make openshift-up.
smoke-openshift: ## Live smoke via oc port-forward to the current cluster
	bash hack/smoke-openshift.sh

##@ Local Keycloak

# See deploy/keycloak/README.md. SKIP_SEED=1 make keycloak-up
keycloak-up: ## Start local Keycloak on :8180 and seed ai-grid tenants
	KEYCLOAK_PORT=$(KEYCLOAK_PORT) CONTAINER_ENGINE="$(CONTAINER_ENGINE)" \
		bash deploy/keycloak/scripts/keycloak-up.sh

keycloak-down: ## Stop and remove the local Keycloak container
	CONTAINER_ENGINE="$(CONTAINER_ENGINE)" bash deploy/keycloak/scripts/keycloak-down.sh

# Re-runnable; also invoked by keycloak-up unless SKIP_SEED=1.
keycloak-seed: ## Seed ai-grid realm + sample Organizations (tenant-a, tenant-b)
	KEYCLOAK_URL=$(KEYCLOAK_URL) bash deploy/keycloak/scripts/keycloak-seed.sh

##@ Cluster / deploy

prereqs: ## Check Podman, Kind, kubectl, grpcurl, Go, clusteradm
	bash hack/check-prereqs.sh

kind-test: ## Full Kind integration test
	bash hack/test-kind.sh

clean-kind: ## Delete the Kind cluster
	kind delete cluster --name "$${KIND_CLUSTER_NAME:-ai-grid-hub}"

# Deploy to the current OpenShift cluster (oc login required).
# Installs RHACM automatically when the ManagedCluster CRD is missing (needs helm).
# See hack/openshift-up.sh for IMAGE, EXTERNAL_KEYCLOAK, BUILD_IMAGE, APPLY_SAMPLES,
# INSTALL_ACM / SKIP_ACM, CHANNEL.
openshift-up: ## Deploy backend to the current oc context
	bash hack/openshift-up.sh

openshift-down: ## Remove OpenShift deploy (does not tear down ACM)
	bash hack/openshift-down.sh

# Install RHACM (OLM + MultiClusterHub) + playground ManagedClusters on the
# current OpenShift cluster. Also invoked by openshift-up when the CRD is missing.
# See deploy/acm-hub/README.md.
# CHANNEL=release-2.13 SKIP_SAMPLES=1 make acm-hub-up
acm-hub-up: ## Install RHACM hub + sample ManagedClusters
	bash deploy/acm-hub/scripts/acm-hub-up.sh

# FORCE_CLEAN=1 make acm-hub-down  (or: bash deploy/acm-hub/scripts/acm-hub-down.sh --force-clean)
# openshift-down does not remove ACM; tear it down separately.
acm-hub-down: ## Tear down ACM hub Helm release
	bash deploy/acm-hub/scripts/acm-hub-down.sh
