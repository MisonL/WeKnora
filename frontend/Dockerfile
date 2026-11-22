# Production image — two-stage build (no host-side npm required).
# Mirrors scripts/build_frontend_dist.sh:
#   npm ci + VITE_IS_DOCKER=true + VITE_FRONTEND_COMMIT
#
# Dist is architecture-independent. The builder uses $BUILDPLATFORM so
# multi-arch CI (linux/amd64,linux/arm64) compiles Vite once on the runner
# instead of qemu-emulating Node, and both runtime images share the same dist.
#
#   docker build --platform linux/amd64 \
#     --build-arg VITE_FRONTEND_COMMIT=$(git rev-parse --short HEAD) \
#     -t wechatopenai/weknora-ui:latest \
#     frontend/
#
# Optional npm mirror (e.g. corporate registry):
#   --build-arg NPM_REGISTRY=https://registry.npmmirror.com
#
# Docker Desktop with a small VM:
#   --build-arg NODE_MAX_OLD_SPACE_SIZE=2048

# ---------------------------------------------------------------------------
# Stage 1: build static assets
# ---------------------------------------------------------------------------
# Pin by digest: compiled JS is shipped in the runtime image, so a floating
# node tag could change production assets. This index includes linux/amd64
# and linux/arm64 (Node 24 / Debian bookworm slim).
FROM --platform=$BUILDPLATFORM node:24-bookworm-slim@sha256:ba849c60be29959425b8734d57b8b4b7d56f98edd9504c9af091d5281095a71e AS builder

WORKDIR /frontend

ARG NPM_REGISTRY=https://registry.npmmirror.com
ARG NODE_MAX_OLD_SPACE_SIZE=4096

ENV VITE_IS_DOCKER=true
# Vite production builds of this SPA routinely exceed Node's default heap.
ENV NODE_OPTIONS="--max-old-space-size=${NODE_MAX_OLD_SPACE_SIZE}"

# The lockfile pins the remote SheetJS archive and its integrity hash.
COPY package.json package-lock.json ./
RUN if [ -n "$NPM_REGISTRY" ]; then \
      npm ci --registry="$NPM_REGISTRY"; \
    else \
      npm ci; \
    fi

COPY . .
# After npm ci so a new commit hash does not bust the dependency layer.
ARG VITE_FRONTEND_COMMIT=unknown
ENV VITE_FRONTEND_COMMIT=${VITE_FRONTEND_COMMIT}
ARG VITE_BASE_PATH=/
ENV VITE_BASE_PATH=${VITE_BASE_PATH}
RUN npm run build

# ---------------------------------------------------------------------------
# Stage 2: nginx runtime
# ---------------------------------------------------------------------------
# Base image pinned by digest for reproducibility and security. Do NOT switch
# back to a floating tag, and do not pin an older base than this one: nginx
# 1.30.7 is the stable release that fixes the "nginx2root" remote code
# execution cluster (CVE-2026-123007 / -123633 / -123634 / -121810 / -113425,
# plus every other 2026 advisory up to 1.30.7), so anything earlier is a
# reachable, unauthenticated RCE on an HTTPS listener. An older Alpine base
# is not required anymore: the one thing a downgrade bought was CentOS 7 /
# old-kernel startup, but a maintained kernel and libseccomp start Alpine 3.24
# without issue (CentOS 7 with kernel 3.10 / libseccomp 2.3 cannot) -- and the
# build handles multiarch via digest index only.
FROM nginx:1.30.7-alpine@sha256:bef154735288bc38d99fac41ae63730684bcc661503673a04a0f9bc526ba8598

COPY --from=builder /frontend/dist /usr/share/nginx/html

COPY nginx.conf /etc/nginx/templates/default.conf.template

# Copied outside templates/ on purpose: envsubst would eat $host and the other
# nginx variables in it.
COPY nginx-api-proxy.conf /etc/nginx/api-proxy.conf
COPY nginx-http.conf /etc/nginx/conf.d/00-weknora-http.conf

COPY docker-entrypoint.sh /docker-entrypoint.sh
RUN chmod +x /docker-entrypoint.sh

ENV MAX_FILE_SIZE_MB=50

EXPOSE 80

ENTRYPOINT ["/docker-entrypoint.sh"]
