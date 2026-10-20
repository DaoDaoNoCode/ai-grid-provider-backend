SHELL := /usr/bin/env bash

GO := go
BIN_DIR := bin
BACKEND_BIN := $(BIN_DIR)/ai-grid-provider-backend

# Container image (Podman preferred). Override with IMAGE=... TAG=dev.
# Default linux/amd64 so OpenShift nodes can run images built on Apple Silicon.
IMAGE ?= quay.io/rh-ee-rjjohnso/ai-grid-provider-backend
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

.PHONY: help all binary build-bin build test unit smoke vet fmt-check lint verify generate \
	image container push build-and-push run \
	keycloak-up keycloak-down keycloak-seed \
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

vet:
	$(GO) vet ./...

fmt-check:
	@files="$$(gofmt -l client cmd gen integration internal)"; \
	if [[ -n "$$files" ]]; then \
		echo "gofmt is required for:"; echo "$$files"; exit 1; \
	fi

lint: fmt-check vet ## gofmt check, go vet; golangci-lint when installed
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not installed; gofmt and go vet completed"; \
	fi

# Regenerates protobuf, gRPC, grpc-gateway, and OpenAPI (OSAC-style buf pipeline).
generate: ## Regenerate protobuf, gateway stubs, and OpenAPI
	cd proto && buf dep update && buf generate
	# Drop legacy monolithic stubs if a prior generate left them behind.
	rm -f gen/ai_grid/v1/ai_grid.pb.go gen/ai_grid/v1/ai_grid_grpc.pb.go gen/ai_grid/v1/ai_grid.pb.gw.go
	mkdir -p internal/restgateway/openapi
	cp docs/openapi/ai_grid.swagger.json internal/restgateway/openapi/ai_grid.swagger.json

verify: build unit lint ## Build binary + unit tests + lint

##@ Container image (Quay)

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

push: ## Push $(IMAGE):$(TAG) to Quay (podman login quay.io first)
	@if [[ -z "$(CONTAINER_ENGINE)" ]]; then echo "podman or docker is required" >&2; exit 1; fi
	@echo "pushing $(IMAGE):$(TAG)"
	$(CONTAINER_ENGINE) push $(IMAGE):$(TAG)

build-and-push: image push ## Build the image, then push to Quay

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
smoke: ## Live gRPC/REST smoke against localhost:8080/:8081
	GRPC_ADDR=$(GRPC_ADDR) HTTP_ADDR=$(HTTP_ADDR) bash hack/smoke-apis.sh
	GRPC_ADDR=$(GRPC_ADDR) bash hack/smoke-managedcluster-crud.sh

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
# See hack/openshift-up.sh for IMAGE, EXTERNAL_KEYCLOAK, BUILD_IMAGE, APPLY_SAMPLES.
openshift-up: ## Deploy backend to the current oc context
	bash hack/openshift-up.sh

openshift-down: ## Remove OpenShift deploy
	bash hack/openshift-down.sh

# Install RHACM (OLM + MultiClusterHub) + playground ManagedClusters on the
# current OpenShift cluster. See deploy/acm-hub/README.md.
# CHANNEL=release-2.13 SKIP_SAMPLES=1 make acm-hub-up
acm-hub-up: ## Install RHACM hub + sample ManagedClusters
	bash deploy/acm-hub/scripts/acm-hub-up.sh

# FORCE_CLEAN=1 make acm-hub-down  (or: bash deploy/acm-hub/scripts/acm-hub-down.sh --force-clean)
acm-hub-down: ## Tear down ACM hub Helm release
	bash deploy/acm-hub/scripts/acm-hub-down.sh
