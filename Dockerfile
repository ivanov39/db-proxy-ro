# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/db-proxy-ro ./cmd/db-proxy-ro

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/db-proxy-ro /usr/local/bin/db-proxy-ro
COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh

ENV DBPROXY_PARAMS_DIR=/params \
    DBPROXY_LOG_DIR=/logs \
    DBPROXY_PGSQL_LISTEN=0.0.0.0:25432 \
    DBPROXY_MYSQL_LISTEN=0.0.0.0:23306
VOLUME ["/params", "/logs"]
EXPOSE 25432 23306
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
