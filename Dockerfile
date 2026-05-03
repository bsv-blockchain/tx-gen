FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /bsv-tx-gen . && mkdir -p /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /bsv-tx-gen /bsv-tx-gen
COPY --from=builder --chown=65532:65532 /data /data
EXPOSE 8080
ENTRYPOINT ["/bsv-tx-gen"]
