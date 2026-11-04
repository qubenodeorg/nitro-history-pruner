# Official Nitro image with two changes:
#   1. /usr/local/bin/nitro rebuilt with the go-ethereum patch that accepts a
#      history-pruned database (see patches/).
#   2. /usr/local/bin/nitro-history-pruner added, so pruning runs from the
#      same image: docker run --rm --entrypoint nitro-history-pruner ...
#
# Everything else (replay machines, prover, contracts) is the untouched
# official image. Built by scripts/build-nitro-image.sh.

ARG BASE_IMAGE
ARG BUILDER_IMAGE

FROM ${BUILDER_IMAGE} AS nitro-builder

FROM golang:1.25-bookworm AS pruner-builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG PRUNER_VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${PRUNER_VERSION}" \
      -o /out/nitro-history-pruner .

FROM ${BASE_IMAGE}
USER root
COPY --from=nitro-builder  /workspace/target/bin/nitro  /usr/local/bin/nitro
COPY --from=pruner-builder /out/nitro-history-pruner     /usr/local/bin/nitro-history-pruner
RUN nitro --version && nitro-history-pruner -version
USER user
