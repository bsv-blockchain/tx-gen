FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /bsv-tx-gen .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /bsv-tx-gen /bsv-tx-gen
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/bsv-tx-gen"]
