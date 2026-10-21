# syntax=docker/dockerfile:1
#
# The ocis-agent sidecar. Build from the repository root:
#
#	docker build -t ocis-agent:dev .
#
# The image holds the static binary only: no configuration and no secrets.
# Every setting comes from the environment at run time; AGENT_MASTER_KEY must
# be passed in (for example with an env_file of mode 0600), never baked in.
# The database lives in /data (a volume); the root filesystem can be read-only.

ARG GO_VERSION=1.26
ARG ALPINE_VERSION=3.22

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
RUN go build -ldflags='-s -w' -o /out/ocis-agent ./cmd/ocis-agent

FROM alpine:${ALPINE_VERSION}
# a fixed non-root uid/gid so a host directory mounted at /data can be chowned to it
RUN addgroup -S -g 10001 ocis-agent \
	&& adduser -S -D -H -u 10001 -G ocis-agent -h /data -s /sbin/nologin ocis-agent \
	&& mkdir -p /data \
	&& chown 10001:10001 /data \
	&& chmod 0700 /data
COPY --from=build /out/ocis-agent /usr/local/bin/ocis-agent
COPY LICENSE NOTICE /usr/share/doc/ocis-agent/

LABEL org.opencontainers.image.title="ocis-agent" \
	org.opencontainers.image.description="Always-on personal agent sidecar for ownCloud Infinite Scale" \
	org.opencontainers.image.source="https://github.com/owncloud/ocis-agent" \
	org.opencontainers.image.licenses="Apache-2.0"

# AGENT_HTTP_ADDR and AGENT_DEBUG_ADDR are fixed here because the health check
# below uses them; change both together if you override them.
ENV AGENT_DATA_DIR=/data \
	AGENT_HTTP_ADDR=:9300 \
	AGENT_DEBUG_ADDR=:9301

USER 10001:10001
WORKDIR /data
VOLUME ["/data"]
# 9300: the REST API under /agent-api/ (route it from the reverse proxy in
# front of oCIS; never route /agent/). 9301: GET /healthz and /readyz only,
# for probes; do not publish it.
EXPOSE 9300 9301

# Liveness of the API role. A worker-only process (AGENT_ROLES=worker) serves
# no API: override the check with the debug listener's /readyz on 9301.
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
	CMD wget -q -T 4 -O /dev/null http://127.0.0.1:9300/agent-api/healthz || exit 1

# SIGTERM starts the graceful shutdown: stop claiming, drain running jobs for
# up to AGENT_SHUTDOWN_DRAIN (default 30s), then requeue them. Give the
# container a stop grace period of at least AGENT_SHUTDOWN_DRAIN + 15s.
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/ocis-agent"]
