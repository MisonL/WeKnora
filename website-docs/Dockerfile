# Build from the repository root with:
#   docker build --platform=linux/amd64 \
#     --build-arg WEBSITE_NGINX_PORT=8080 \
#     -t weknora-site website-docs
# No host Node.js installation or prebuilt static-site/ is required.
# WEBSITE_NGINX_PORT is written into default.conf at build time. The intranet
# platform starts `nginx; sleep infinity` and does not run docker-entrypoint.sh.
FROM --platform=$BUILDPLATFORM node:24-bookworm-slim@sha256:ba849c60be29959425b8734d57b8b4b7d56f98edd9504c9af091d5281095a71e AS builder
RUN apt-get update \
    && apt-get install -y --no-install-recommends git \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /build
ENV NEXT_TELEMETRY_DISABLED=1

# Install both locked dependency sets before copying source for layer caching.
COPY package.json package-lock.json ./
COPY homepage/package.json homepage/package-lock.json ./homepage/
RUN npm run setup

COPY . .
RUN npm run build

# Pin the runtime base. Do NOT switch back to a floating `nginx:stable-alpine`
# tag: it drifts with every stable release and the digest below is what was
# audited. 1.30.7 is the stable release that fixes the 2026 "nginx2root"
# remote-code-execution cluster (CVE-2026-123007 and friends) together with
# every other advisory up to that point, so a downgrade reopens reachable
# vulnerabilities. This digest is nginx 1.30.7 / Alpine 3.24.4; the host
# therefore needs a kernel/libseccomp new enough to start it (CentOS 7 with
# kernel 3.10 and libseccomp 2.3 cannot, which is why the older 1.30.3 pin
# existed -- any maintained distro runs it).
FROM nginx:1.30.7-alpine@sha256:bef154735288bc38d99fac41ae63730684bcc661503673a04a0f9bc526ba8598 AS runtime
# Some deployment platforms wrap the startup command with /bin/bash -c.
RUN apk add --no-cache bash
# Baked into default.conf below. Entrypoint still honors a runtime override
# when it actually runs; the platform path does not.
ARG WEBSITE_NGINX_PORT=80
ENV WEBSITE_NGINX_PORT=${WEBSITE_NGINX_PORT}
COPY deploy/nginx.conf /etc/nginx/conf.d/default.conf
RUN mkdir -p /etc/nginx/templates \
    && sed 's/listen 80;/listen ${WEBSITE_NGINX_PORT};/' /etc/nginx/conf.d/default.conf \
      > /etc/nginx/templates/default.conf.template \
    && sed "s/listen 80;/listen ${WEBSITE_NGINX_PORT};/" /etc/nginx/conf.d/default.conf \
      > /etc/nginx/conf.d/default.conf.baked \
    && mv /etc/nginx/conf.d/default.conf.baked /etc/nginx/conf.d/default.conf \
    && grep -q "listen ${WEBSITE_NGINX_PORT};" /etc/nginx/conf.d/default.conf \
    && rm -rf /usr/share/nginx/html/*
COPY --from=builder /build/static-site/ /usr/share/nginx/html/
COPY docker-entrypoint.sh /docker-entrypoint.sh
RUN chmod +x /docker-entrypoint.sh
EXPOSE ${WEBSITE_NGINX_PORT}
ENTRYPOINT ["/docker-entrypoint.sh"]
