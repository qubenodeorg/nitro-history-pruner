#!/usr/bin/env bash
# Build a Nitro image that accepts a history-pruned database.
#
# Usage: scripts/build-nitro-image.sh [NITRO_TAG] [BASE_IMAGE] [OUTPUT_IMAGE]
#   NITRO_TAG     Nitro git tag to build            (default: v3.11.3)
#   BASE_IMAGE    official image for that tag       (default: offchainlabs/nitro-node:v3.11.3-beb2108)
#   OUTPUT_IMAGE  resulting image name              (default: nitro-node-pruned:<NITRO_TAG>)
#
# Needs docker, git and ~40 GB of free disk. Builds Nitro's own `node-builder`
# stage from source (Rust prover, WASM, contracts), so the first build takes a
# while. Run it on an amd64 host for amd64 nodes.
set -euo pipefail

NITRO_TAG="${1:-v3.11.3}"
BASE_IMAGE="${2:-offchainlabs/nitro-node:v3.11.3-beb2108}"
OUTPUT_IMAGE="${3:-nitro-node-pruned:${NITRO_TAG}}"

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
PATCH_DIR="${REPO_DIR}/patches/${NITRO_TAG}"
WORK_DIR="${WORK_DIR:-${REPO_DIR}/.build/nitro-${NITRO_TAG}}"
BUILDER_IMAGE="nitro-pruned-builder:${NITRO_TAG}"
PRUNER_VERSION="$(git -C "${REPO_DIR}" describe --tags --always --dirty 2>/dev/null || echo dev)"

if [[ ! -d "${PATCH_DIR}" ]]; then
  echo "No patches for ${NITRO_TAG} in ${PATCH_DIR}." >&2
  echo "Port patches/<previous tag>/ to the new go-ethereum first." >&2
  exit 1
fi

if [[ ! -d "${WORK_DIR}/.git" ]]; then
  echo ">> Cloning Nitro ${NITRO_TAG}"
  mkdir -p "$(dirname "${WORK_DIR}")"
  git clone --depth 1 --branch "${NITRO_TAG}" --recurse-submodules --shallow-submodules \
    https://github.com/OffchainLabs/nitro.git "${WORK_DIR}"
fi

echo ">> Applying patches"
git -C "${WORK_DIR}/go-ethereum" checkout -- .
for p in "${PATCH_DIR}"/go-ethereum-*.patch; do
  echo "   ${p##*/}"
  git -C "${WORK_DIR}/go-ethereum" apply --exclude='*_test.go' "${p}"
done

# Nitro's Dockerfile copies this file in; its CI normally creates it.
echo "${NITRO_TAG}-pruned" > "${WORK_DIR}/.nitro-tag.txt"

echo ">> Building Nitro node-builder stage (slow on first run)"
DOCKER_BUILDKIT=1 docker build \
  --target node-builder \
  --build-arg version="${NITRO_TAG}-pruned" \
  --build-arg datetime="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --build-arg modified=true \
  -t "${BUILDER_IMAGE}" \
  "${WORK_DIR}"

echo ">> Building ${OUTPUT_IMAGE}"
DOCKER_BUILDKIT=1 docker build \
  -f "${REPO_DIR}/docker/Dockerfile" \
  --build-arg BASE_IMAGE="${BASE_IMAGE}" \
  --build-arg BUILDER_IMAGE="${BUILDER_IMAGE}" \
  --build-arg PRUNER_VERSION="${PRUNER_VERSION}" \
  -t "${OUTPUT_IMAGE}" \
  "${REPO_DIR}"

echo ">> Built ${OUTPUT_IMAGE}"
