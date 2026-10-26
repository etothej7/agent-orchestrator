#!/usr/bin/env bash
# Cross-compile the Go `ao` binary (backend/cmd/ao) for every supported
# platform and drop each into the matching platform package's bin/ dir.
#
# Run this from any cwd before `npm publish`. It is the ONLY way the binaries
# get into the platform packages; they are gitignored and produced here, then
# shipped in each npm tarball via that package's `files` entry.
#
# CGO-free build (modernc.org/sqlite driver) so cross-compilation needs no C
# toolchain. Prod build: no -ldflags, so cli.releaseRepo keeps its default
# (AgentWrapper/agent-orchestrator).
set -euo pipefail

# Repo layout: this script lives at <repo>/packages/build-binaries.sh.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
BACKEND_DIR="${REPO_ROOT}/backend"

# pkg_dir : npm_os : npm_arch : GOOS : GOARCH : bin_name
TARGETS=(
  "ao-darwin-arm64:darwin:arm64:darwin:arm64:ao"
  "ao-darwin-x64:darwin:x64:darwin:amd64:ao"
  "ao-win32-x64:win32:x64:windows:amd64:ao.exe"
  "ao-linux-x64:linux:x64:linux:amd64:ao"
)

# Shipped binaries are built with this Go release or a later stable one, using
# the toolchain installed here (GOTOOLCHAIN=local below): never one downloaded
# on the fly.
REQUIRED_GO="1.27.3"
go_version="$(GOTOOLCHAIN=local go env GOVERSION)"
go_version="${go_version#go}"
if [[ ! "${go_version}" =~ ^[0-9]+\.[0-9]+(\.[0-9]+)?$ ]] || \
   [[ "$(printf '%s\n%s\n' "${REQUIRED_GO}" "${go_version}" | sort -V | head -n1)" != "${REQUIRED_GO}" ]]; then
  echo "Go ${REQUIRED_GO}+ (stable) required, found '${go_version}' - upgrade at https://go.dev/dl/" >&2
  exit 1
fi

echo "Building ao binaries from ${BACKEND_DIR}/cmd/ao (go${go_version})"
for t in "${TARGETS[@]}"; do
  IFS=":" read -r pkg npm_os npm_arch goos goarch bin <<<"$t"
  out="${SCRIPT_DIR}/${pkg}/bin/${bin}"
  mkdir -p "${SCRIPT_DIR}/${pkg}/bin"
  echo "  -> ${pkg} (GOOS=${goos} GOARCH=${goarch}) -> bin/${bin}"
  (cd "${BACKEND_DIR}" && CGO_ENABLED=0 GOTOOLCHAIN=local GOOS="${goos}" GOARCH="${goarch}" \
    go build -o "${out}" ./cmd/ao)
  chmod 0755 "${out}"
done

echo "Done. Built binaries:"
for t in "${TARGETS[@]}"; do
  IFS=":" read -r pkg _ _ _ _ bin <<<"$t"
  file "${SCRIPT_DIR}/${pkg}/bin/${bin}"
done
