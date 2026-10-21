# Cross-compile for OpenShift (typically linux/amd64).
# Always pass TARGETOS/TARGETARCH from the Makefile / openshift-up.sh;
# Podman on Apple Silicon does not always inject TARGETARCH for multi-stage builds.
#
# Red Hat UBI images, pinned by tag and digest (Dependabot bumps both). go-toolset's Go must be
# at least go.mod's go line. The runtime is ubi-minimal, not ubi-micro, because ubi-micro has
# no CA bundle and the server calls the hub and Keycloak over TLS.
ARG TARGETOS=linux
ARG TARGETARCH=amd64

FROM --platform=$BUILDPLATFORM registry.access.redhat.com/ubi9/go-toolset:1.26.10-1792592174@sha256:d4d25f236c2b3cd27eb407e3b3478bef0b75d644ee6d11772217f537be926b11 AS build
ARG TARGETOS
ARG TARGETARCH
# go-toolset runs as 1001 with its home as the working directory.
WORKDIR /opt/app-root/src
COPY --chown=1001:0 go.mod go.sum ./
RUN go mod download
COPY --chown=1001:0 . .
RUN test -n "${TARGETARCH}" && test -n "${TARGETOS}"
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
	go build -trimpath -ldflags='-s -w' -o /tmp/out/ai-grid-provider-backend \
	./cmd/ai-grid-provider-backend \
	&& echo "built GOOS=${TARGETOS} GOARCH=${TARGETARCH}"

# Final image platform comes from `podman build --platform=...`.
FROM registry.access.redhat.com/ubi9/ubi-minimal:9.8-1791898448@sha256:c4c285d42941ebb2bd4da878479008e1e4536bb599aad1dbe568f89de9053661
# Owned by root and world-readable, so OpenShift's arbitrary UID (group 0) can run it.
COPY --from=build --chown=0:0 /tmp/out/ai-grid-provider-backend /ai-grid-provider-backend
USER 1001
EXPOSE 8080 8081
ENTRYPOINT ["/ai-grid-provider-backend"]
