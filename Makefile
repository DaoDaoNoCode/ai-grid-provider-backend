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

.PHONY: all build test vet fmt-check lint verify generate image push build-and-push prereqs kind-test clean-kind openshift-up openshift-down acm-hub-up acm-hub-down

all: verify

build:
	mkdir -p $(BIN_DIR)
	$(GO) build -buildvcs=false -trimpath -o $(BACKEND_BIN) ./cmd/ai-grid-provider-backend

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt-check:
	@files="$$(gofmt -l client cmd gen integration internal)"; \
	if [[ -n "$$files" ]]; then \
		echo "gofmt is required for:"; echo "$$files"; exit 1; \
	fi

lint: fmt-check vet
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not installed; gofmt and go vet completed"; \
	fi

# Regenerates protobuf, gRPC, grpc-gateway, and OpenAPI (OSAC-style buf pipeline).
generate:
	cd proto && buf dep update && buf generate
	# Drop legacy monolithic stubs if a prior generate left them behind.
	rm -f gen/ai_grid/v1/ai_grid.pb.go gen/ai_grid/v1/ai_grid_grpc.pb.go gen/ai_grid/v1/ai_grid.pb.gw.go
	mkdir -p internal/restgateway/openapi
	cp docs/openapi/ai_grid.swagger.json internal/restgateway/openapi/ai_grid.swagger.json

verify: build test lint

# Build the container image with Podman (falls back to docker if podman is missing).
# PLATFORM is os/arch (default linux/amd64). TARGETARCH is passed explicitly because
# Podman on Apple Silicon often leaves TARGETARCH unset, producing Exec format error on OpenShift.
image:
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

# Push $(IMAGE):$(TAG). Requires prior `podman login quay.io` (or docker login).
push:
	@if [[ -z "$(CONTAINER_ENGINE)" ]]; then echo "podman or docker is required" >&2; exit 1; fi
	$(CONTAINER_ENGINE) push $(IMAGE):$(TAG)

# Build then push the container image.
build-and-push: image push

prereqs:
	bash hack/check-prereqs.sh

kind-test:
	bash hack/test-kind.sh

clean-kind:
	kind delete cluster --name "$${KIND_CLUSTER_NAME:-ai-grid-hub}"

# Deploy to the current OpenShift cluster (oc login required).
# See hack/openshift-up.sh for IMAGE, EXTERNAL_KEYCLOAK, BUILD_IMAGE, APPLY_SAMPLES.
openshift-up:
	bash hack/openshift-up.sh

openshift-down:
	bash hack/openshift-down.sh

# Install RHACM (OLM + MultiClusterHub) + playground ManagedClusters on the
# current OpenShift cluster. See deploy/acm-hub/README.md.
# CHANNEL=release-2.13 SKIP_SAMPLES=1 make acm-hub-up
acm-hub-up:
	bash deploy/acm-hub/scripts/acm-hub-up.sh

# FORCE_CLEAN=1 make acm-hub-down  (or: bash deploy/acm-hub/scripts/acm-hub-down.sh --force-clean)
acm-hub-down:
	bash deploy/acm-hub/scripts/acm-hub-down.sh
