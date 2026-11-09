#!/usr/bin/env bash
# Build an incremental image on the target host from a locally built image.
#
# Why this exists: the production ECS is reached over a slow link, and a full
# `docker save | ssh | docker load` moves the whole 2.45 GB image even when a
# rebuild only changed the Go binary. This script ships just the files that
# differ from an image the host already has, and assembles a new image there
# with `FROM <base>` + `COPY`. Nothing is compiled on the host.
#
# Usage:
#   build-delta.sh <new-image> <base-image> <target-tag> [ssh-host] [build-arg...]
#
# Example:
#   build-delta.sh weknora-app:v0.9.0-oss-sts-r2 \
#                  weknora-app:v0.9.0-oss-sts \
#                  weknora-app:v0.9.0-oss-sts-r2 nora
#
# The base image must exist on the target host under the same tag. Paths that
# differ are detected by size+sha256; only those are shipped.
#
# The overlay is additive — FROM base + COPY, which merges directories and never
# deletes — so the script refuses (exit 3) unless the two images differ only in
# file *contents* inside PATHS:
#   - image metadata (ENV/ENTRYPOINT/CMD/USER/WORKDIR/PORTS/VOLUMES) is inherited
#     from the base and would be silently dropped if it changed;
#   - a file deleted in the new image would survive the overlay;
#   - a new file outside PATHS would never be shipped.
# After building, the host diffs the result's file list against the source image
# and removes the tag (exit 4) if they disagree, so a delta deploy can never
# report success for an image that is not the one that was built.
set -euo pipefail

NEW_IMAGE=${1:?new image tag required}
BASE_IMAGE=${2:?base image tag required}
TARGET_TAG=${3:?target tag required}
HOST=${4:-nora}
shift 4 2>/dev/null || shift 3 2>/dev/null || true
EXTRA_ARGS=("$@")

# Files that a rebuild can legitimately change. Paths are relative to the
# image root. Keep this list small: every entry is copied locally twice.
PATHS=(
  app/WeKnora
  app/migrations
  app/scripts
  app/config
  app/dataset/samples
)

WORK=$(mktemp -d)
CTX="$WORK/context"
mkdir -p "$CTX"
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT

new_cid=$(docker create "$NEW_IMAGE")
base_cid=$(docker create "$BASE_IMAGE")
trap 'docker rm -f "$new_cid" "$base_cid" >/dev/null 2>&1 || true; cleanup' EXIT

# The delta image is `FROM base` + `COPY` of changed paths. Docker's COPY merges
# directory trees and never deletes, so it is only equivalent to the real image
# when nothing was removed, nothing was added outside PATHS, and the image
# metadata is unchanged. Refuse to build otherwise: an additive overlay that
# silently keeps a deleted file (or misses a new directory) is worse than a slow
# full transfer, because both look like a successful deploy.
die_full_transfer() {
  echo "build-delta: REFUSING incremental build — $*" >&2
  echo "build-delta: ship the full image instead (docker save | gzip | ssh docker load)." >&2
  exit 3
}

# 1. Image metadata is inherited from the base, so any change there (new env
#    var, entrypoint, user, exposed port) would be silently dropped.
meta() {
  docker image inspect "$1" --format \
    '{{json .Config.Env}}|{{json .Config.Entrypoint}}|{{json .Config.Cmd}}|{{.Config.User}}|{{.Config.WorkingDir}}|{{json .Config.ExposedPorts}}|{{json .Config.Volumes}}'
}
if [[ "$(meta "$NEW_IMAGE")" != "$(meta "$BASE_IMAGE")" ]]; then
  die_full_transfer "image metadata (ENV/ENTRYPOINT/CMD/USER/WORKDIR/PORTS/VOLUMES) differs"
fi

# 2. Every tracked path must exist in both images. A path that disappeared from
#    the new image would otherwise keep the base copy forever (docker cp fails
#    and the loop used to skip it silently).
for p in "${PATHS[@]}"; do
  docker cp "$new_cid:/$p" "$WORK/probe" >/dev/null 2>&1 \
    || die_full_transfer "path /$p is missing from $NEW_IMAGE"
  rm -rf "$WORK/probe"
done

# 3. Compare the actual file lists. COPY can add files but never remove them, and
#    it only carries the tracked paths, so:
#      - a file that exists in the base but not in the new image would survive
#        the overlay (deleted migration, renamed config, dropped asset);
#      - a new file outside PATHS would never be shipped.
#    Both produce an image that looks deployed but is not what was built.
manifest() {
  docker run --rm --entrypoint sh "$1" -c \
    'cd / && find . -xdev \( -path ./proc -o -path ./sys -o -path ./dev -o -path ./tmp \) -prune -o -print' \
    | sort
}
manifest "$NEW_IMAGE" > "$WORK/new.manifest"
manifest "$BASE_IMAGE" > "$WORK/base.manifest"

removed=$(comm -23 "$WORK/base.manifest" "$WORK/new.manifest" | head -20)
if [[ -n "$removed" ]]; then
  echo "build-delta: files present in $BASE_IMAGE but absent from $NEW_IMAGE:" >&2
  printf '  %s\n' $removed >&2
  die_full_transfer "the new image deletes files, which an additive overlay cannot express"
fi

while IFS= read -r added; do
  [[ -n "$added" ]] || continue
  covered=0
  for p in "${PATHS[@]}"; do
    [[ "$added" == "./$p" || "$added" == "./$p/"* ]] && { covered=1; break; }
  done
  [[ $covered -eq 1 ]] || die_full_transfer "new path $added is outside the tracked paths and would not be shipped"
done < <(comm -13 "$WORK/base.manifest" "$WORK/new.manifest")

changed=()
for p in "${PATHS[@]}"; do
  name=$(basename "$p")
  docker cp "$new_cid:/$p" "$WORK/new-$name" 2>/dev/null || continue
  docker cp "$base_cid:/$p" "$WORK/base-$name" 2>/dev/null || { changed+=("$p"); cp -a "$WORK/new-$name" "$CTX/$name"; continue; }
  if diff -rq "$WORK/base-$name" "$WORK/new-$name" >/dev/null 2>&1; then
    :
  else
    changed+=("$p")
    cp -a "$WORK/new-$name" "$CTX/$name"
  fi
done

if [[ ${#changed[@]} -eq 0 ]]; then
  echo "build-delta: no file differences between $BASE_IMAGE and $NEW_IMAGE" >&2
  exit 2
fi

{
  echo "FROM $BASE_IMAGE"
  for p in "${PATHS[@]}"; do
    name=$(basename "$p")
    [[ -e "$CTX/$name" ]] || continue
    if [[ -d "$CTX/$name" ]]; then
      echo "COPY $name /$(dirname "$p")/$name"
    else
      echo "COPY $name /$p"
    fi
  done
} > "$CTX/Dockerfile"

echo "build-delta: changed paths: ${changed[*]}"
echo "build-delta: context size: $(du -sh "$CTX" | awk '{print $1}')"

# Ship the expected file list so the host can verify what it actually built.
# The pre-checks argue that this overlay equals the real image; this is the check
# that proves it, and it is the only thing standing between a COPY that silently
# missed a directory and a deploy that reports success.
cp "$WORK/new.manifest" "$CTX/expected.manifest"

ARCHIVE="$WORK/delta.tar.gz"
tar -C "$CTX" -czf "$ARCHIVE" .
echo "build-delta: archive size: $(du -h "$ARCHIVE" | awk '{print $1}')"

remote_dir="/tmp/nora-delta-$$"
ssh "$HOST" "rm -rf '$remote_dir' && mkdir -p '$remote_dir'"
scp -q "$ARCHIVE" "$HOST:$remote_dir/delta.tar.gz"
ssh "$HOST" "set -e
  cd '$remote_dir'
  tar xzf delta.tar.gz
  rm -f delta.tar.gz
  sudo -n docker build ${EXTRA_ARGS[*]:-} -t '$TARGET_TAG' . >/dev/null
  sudo -n docker run --rm --entrypoint sh '$TARGET_TAG' -c 'cd / && find . -xdev \( -path ./proc -o -path ./sys -o -path ./dev -o -path ./tmp \) -prune -o -print' | sort > actual.manifest
  if ! diff -q expected.manifest actual.manifest >/dev/null; then
    echo 'build-delta: BUILT IMAGE DOES NOT MATCH THE SOURCE IMAGE' >&2
    diff expected.manifest actual.manifest | head -30 >&2
    sudo -n docker rmi -f '$TARGET_TAG' >/dev/null 2>&1 || true
    rm -rf '$remote_dir'
    exit 4
  fi
  rm -rf '$remote_dir'"
echo "build-delta: built $TARGET_TAG on $HOST and verified its file list against $NEW_IMAGE"
