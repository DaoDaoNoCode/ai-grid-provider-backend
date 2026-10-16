# Cross-compile for OpenShift (typically linux/amd64).
# Always pass TARGETOS/TARGETARCH from the Makefile / openshift-up.sh;
# Podman on Apple Silicon does not always inject TARGETARCH for multi-stage builds.
ARG TARGETOS=linux
ARG TARGETARCH=amd64

FROM --platform=$BUILDPLATFORM golang:1.26.3 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN test -n "${TARGETARCH}" && test -n "${TARGETOS}"
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
	go build -trimpath -ldflags='-s -w' -o /out/ai-grid-provider-backend \
	./cmd/ai-grid-provider-backend \
	&& echo "built GOOS=${TARGETOS} GOARCH=${TARGETARCH}"

# Final image platform comes from `podman build --platform=...`.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ai-grid-provider-backend /ai-grid-provider-backend
EXPOSE 8080 8081
ENTRYPOINT ["/ai-grid-provider-backend"]
