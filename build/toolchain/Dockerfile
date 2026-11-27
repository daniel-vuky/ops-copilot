# Ops Copilot toolchain image (ADR 0001 §14).
#
# Holds Go, Node, pnpm and golangci-lint at pinned versions so the host needs
# only Docker, make and git. The Makefile tags this image by the git hash of
# this file, so any edit here produces a new tag and a rebuild.
#
# Every download is pinned by version and verified by sha256. Supported
# architectures: amd64 and arm64.
#
# Needs BuildKit (the default builder in current Docker), which sets
# TARGETARCH. The legacy builder leaves it empty and the build fails with
# "unsupported TARGETARCH".
#
# Where each sha256 comes from when bumping a version:
# - Node: https://nodejs.org/dist/v<version>/SHASUMS256.txt
# - pnpm: the asset digest of the release assets on the pnpm/pnpm GitHub
#   releases page
# - golangci-lint: the golangci-lint-<version>-checksums.txt release asset

FROM golang:1.27.4-trixie@sha256:430985542100b871e108ccc00e8dcc5eef08ca14ee5825e98ebf563d8ffec4ee

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

ARG TARGETARCH

# Keep equal to `engines.node` in web/package.json (without the leading v).
ARG NODE_VERSION=v26.13.1
ARG NODE_SHA256_X64=518674de2b621728a5e4486f5fb3b7d2900bb3cc4aa97969edaa463db99f9778
ARG NODE_SHA256_ARM64=80a330b35415382878df18d41afbd106ea89bfb3be0087d6608e289a14c5cc7d

# Keep equal to `packageManager` in web/package.json; regenerate
# web/pnpm-lock.yaml when bumping.
ARG PNPM_VERSION=12.21.0
ARG PNPM_SHA256_X64=84c4773bcf4a217f4083cd7b2a91e4cee6103cf42426c1282ef37d308f55a410
ARG PNPM_SHA256_ARM64=6c5913a48c93b3a556b71c3bd9bf12c6bf9fbfbbbd2dcbd1d52a85c539751b9e

ARG GOLANGCI_LINT_VERSION=2.15.0
ARG GOLANGCI_LINT_SHA256_AMD64=9b7fe6a6bf6334b98decf8d86b11415ea7889afdabdd1959fdce23af2a9ebcea
ARG GOLANGCI_LINT_SHA256_ARM64=b4d8f17068b35ee6060cc2f69a16fb48740d706fbba81661c318649c114a6998

# Node: official linux tarball, extracted to /usr/local/node.
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) arch=x64;   sha="${NODE_SHA256_X64}" ;; \
      arm64) arch=arm64; sha="${NODE_SHA256_ARM64}" ;; \
      *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    file="node-${NODE_VERSION}-linux-${arch}.tar.gz"; \
    cd /tmp; \
    curl -fsSL --retry 3 --retry-connrefused --proto '=https' --tlsv1.2 -o "${file}" "https://nodejs.org/dist/${NODE_VERSION}/${file}"; \
    echo "${sha}  ${file}" | sha256sum -c -; \
    mkdir -p /usr/local/node; \
    tar -xzf "${file}" -C /usr/local/node --strip-components=1 --no-same-owner; \
    rm -f "${file}"

# pnpm: standalone release tarball (Node 25+ ships no corepack). The binary
# needs its sibling dist/ tree, so the whole tarball goes to /usr/local/pnpm
# and that directory is put on PATH.
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) arch=x64;   sha="${PNPM_SHA256_X64}" ;; \
      arm64) arch=arm64; sha="${PNPM_SHA256_ARM64}" ;; \
      *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    file="pnpm-linux-${arch}.tar.gz"; \
    cd /tmp; \
    curl -fsSL --retry 3 --retry-connrefused --proto '=https' --tlsv1.2 -o "${file}" "https://github.com/pnpm/pnpm/releases/download/v${PNPM_VERSION}/${file}"; \
    echo "${sha}  ${file}" | sha256sum -c -; \
    mkdir -p /usr/local/pnpm; \
    tar -xzf "${file}" -C /usr/local/pnpm --no-same-owner; \
    rm -f "${file}"

# golangci-lint: release tarball, only the binary goes to /usr/local/bin.
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) sha="${GOLANGCI_LINT_SHA256_AMD64}" ;; \
      arm64) sha="${GOLANGCI_LINT_SHA256_ARM64}" ;; \
      *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    dir="golangci-lint-${GOLANGCI_LINT_VERSION}-linux-${TARGETARCH}"; \
    file="${dir}.tar.gz"; \
    cd /tmp; \
    curl -fsSL --retry 3 --retry-connrefused --proto '=https' --tlsv1.2 -o "${file}" "https://github.com/golangci/golangci-lint/releases/download/v${GOLANGCI_LINT_VERSION}/${file}"; \
    echo "${sha}  ${file}" | sha256sum -c -; \
    tar -xzf "${file}" -C /usr/local/bin --strip-components=1 --no-same-owner "${dir}/golangci-lint"; \
    rm -f "${file}"

# Caches live in the ops-copilot-cache volume mounted at /cache. A new named
# volume copies this directory's ownership and mode, so any uid can write.
RUN mkdir -p /cache/go-build /cache/go-mod /cache/golangci-lint /cache/pnpm-store \
    && chmod -R 1777 /cache

WORKDIR /src

ENV CGO_ENABLED=0 \
    GOFLAGS=-buildvcs=false \
    GOCACHE=/cache/go-build \
    GOMODCACHE=/cache/go-mod \
    GOLANGCI_LINT_CACHE=/cache/golangci-lint \
    pnpm_config_store_dir=/cache/pnpm-store \
    pnpm_config_verify_deps_before_run=error \
    pnpm_config_update_notifier=false \
    PATH=/usr/local/node/bin:/usr/local/pnpm:${PATH}

# Leave /tmp empty so no root-owned dot-dirs are baked into $HOME.
RUN rm -rf /tmp/* /tmp/.[!.]*

# HOME=/tmp works for any uid, including one with no passwd entry (CI).
ENV HOME=/tmp
