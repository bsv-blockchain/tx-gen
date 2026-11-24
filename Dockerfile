# ------------------------------------------------------------------------------
#  tx-gen container image
#
#  Build:  docker build -t tx-gen .
#  Run:    docker run -p 8080:8080 -v tx-gen-data:/data \
#            -e ADMIN_TOKEN=... -e INSTANCE_ID=... tx-gen
#
#  Base images are pinned by digest; Dependabot keeps them current.
# ------------------------------------------------------------------------------

# ---- Build: compile a static binary on the build host's native platform ----
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/

ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}" \
      -o /out/tx-gen ./cmd/tx-gen \
 && mkdir -p /out/data

# ---- Runtime: distroless, no shell, non-root -------------------------------
FROM gcr.io/distroless/static-debian13:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5

COPY --from=builder /out/tx-gen /tx-gen
COPY --from=builder --chown=65532:65532 /out/data /data

# Keep the BoltDB state on the volume so restarts resume the same chains.
ENV STATE_PATH=/data/state.db
VOLUME ["/data"]
EXPOSE 8080
USER nonroot:nonroot

ENTRYPOINT ["/tx-gen"]
