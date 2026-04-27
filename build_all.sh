#!/usr/bin/env bash
set -euo pipefail

BINARY="jacred"
CMD="./cmd"
OUT="./Dist"

# Git metadata for -ldflags
GIT_SHA=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")
GIT_BRANCH=$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo "unknown")
BUILD_DATE=$(date -u +"%Y-%m-%d %H:%M:%S UTC")
VERSION=$(git describe --tags --always 2>/dev/null || echo "dev")

LDFLAGS="-s -w \
  -X 'jacred/server.GitSha=${GIT_SHA}' \
  -X 'jacred/server.GitBranch=${GIT_BRANCH}' \
  -X 'jacred/server.BuildDate=${BUILD_DATE}' \
  -X 'jacred/server.Version=${VERSION}'"

# Platforms: OS/ARCH
TARGETS=(
  linux/amd64
  linux/arm64
  linux/arm
  linux/386
  darwin/amd64
  darwin/arm64
  windows/amd64
  windows/arm64
  windows/386
  freebsd/amd64
  freebsd/arm64
)

rm -fr ${OUT}/*
mkdir -p "${OUT}"

OK=0
FAIL=0

go mod tidy

for TARGET in "${TARGETS[@]}"; do
  GOOS="${TARGET%/*}"
  GOARCH="${TARGET#*/}"

  NAME="${BINARY}-${GOOS}-${GOARCH}"
  [[ "${GOOS}" == "windows" ]] && NAME="${NAME}.exe"

  OUTFILE="${OUT}/${NAME}"

  printf "  %-30s" "${TARGET}"

  if CGO_ENABLED=0 GOOS="${GOOS}" GOARCH="${GOARCH}" \
      go build -trimpath -ldflags "${LDFLAGS}" -o "${OUTFILE}" "${CMD}" 2>/tmp/build_err; then
    SIZE=$(du -sh "${OUTFILE}" 2>/dev/null | cut -f1)
    echo "OK  (${SIZE})"
    (( OK++ )) || true
  else
    echo "FAILED"
    cat /tmp/build_err | sed 's/^/    /'
    (( FAIL++ )) || true
  fi
done

echo ""
echo "done: ${OK} ok, ${FAIL} failed  →  ${OUT}/"
ls -lh "${OUT}"/

# ── Archive ───────────────────────────────────────────────────────────────────

ARCHIVE="jacred-${VERSION}-${GIT_SHA}.tar.gz"

echo ""
echo "creating ${ARCHIVE} ..."

tar -czf "${ARCHIVE}" \
  -C "${OUT}" . \
  -C "$(pwd)" init.yaml init.yaml.example
mv -f ${ARCHIVE} ${OUT}/${ARCHIVE}
