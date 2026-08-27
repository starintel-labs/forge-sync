FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY migrations ./migrations
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/forge-sync ./cmd/forge-sync

FROM alpine:3.21
RUN apk add --no-cache ca-certificates git tzdata \
    && addgroup -S forge-sync \
    && adduser -S -G forge-sync -h /var/lib/forge-sync -s /sbin/nologin forge-sync
COPY --from=build /out/forge-sync /usr/local/bin/forge-sync
USER forge-sync
WORKDIR /var/lib/forge-sync
ENV FORGE_SYNC_STATE_PATH=/var/lib/forge-sync/forge-sync.db
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["forge-sync"]
CMD ["serve"]
