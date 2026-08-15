FROM golang:1.23-alpine AS build
WORKDIR /src

# Dependencies first so a source-only change reuses the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO_ENABLED=0 works because modernc.org/sqlite is a pure-Go SQLite; that is
# the whole reason it was chosen over mattn/go-sqlite3 for a static arm64 image.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/capgo-selfhost .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata wget \
    && adduser -D -u 10001 capgo
COPY --from=build /out/capgo-selfhost /usr/local/bin/capgo-selfhost

ENV DATA_DIR=/data PORT=8080
RUN mkdir -p /data && chown capgo:capgo /data
VOLUME /data
USER capgo
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/capgo-selfhost"]
