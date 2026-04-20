#!/usr/bin/env bash

set -Eeuo pipefail

STATE_ENV_FILE="${STATE_ENV_FILE:-.github/scripts/sync-upstream-release.env}"
FAILED_STEP="init"
UPSTREAM_TAG=""
TAG_ALREADY_EXISTS="false"
DRY_RUN="$(printf '%s' "${DRY_RUN:-false}" | tr '[:upper:]' '[:lower:]')"

if [[ "${DRY_RUN}" != "true" && "${DRY_RUN}" != "false" ]]; then
  echo "DRY_RUN must be true or false" >&2
  exit 1
fi

write_state_env() {
  mkdir -p "$(dirname "${STATE_ENV_FILE}")"
  {
    printf 'UPSTREAM_TAG=%s\n' "${UPSTREAM_TAG}"
    printf 'FAILED_STEP=%s\n' "${FAILED_STEP}"
    printf 'TAG_ALREADY_EXISTS=%s\n' "${TAG_ALREADY_EXISTS}"
    printf 'DRY_RUN=%s\n' "${DRY_RUN}"
  } >"${STATE_ENV_FILE}"
}

on_error() {
  local exit_code=$?
  write_state_env
  exit "${exit_code}"
}

trap on_error ERR

set_step() {
  FAILED_STEP="$1"
  echo "[sync-upstream-release] ${FAILED_STEP}"
}

UPSTREAM_REPO_URL="${UPSTREAM_REPO_URL:-https://github.com/router-for-me/CLIProxyAPI.git}"

if ! git remote get-url upstream >/dev/null 2>&1; then
  git remote add upstream "${UPSTREAM_REPO_URL}"
fi

set_step "lookup-upstream-tag"
UPSTREAM_TAG="$(git ls-remote --tags --refs upstream | awk '{print $2}' | sed 's#refs/tags/##' | sort -V | tail -n1)"
if [[ -z "${UPSTREAM_TAG}" ]]; then
  echo "No tags found on upstream remote" >&2
  exit 1
fi

set_step "check-existing-tag"
if git ls-remote --exit-code --tags origin "refs/tags/${UPSTREAM_TAG}" >/dev/null 2>&1; then
  TAG_ALREADY_EXISTS="true"
  FAILED_STEP=""
  write_state_env
  echo "[sync-upstream-release] Tag ${UPSTREAM_TAG} already exists on origin, skipping sync"
  exit 0
fi

set_step "fetch-refs"
git fetch --prune --tags upstream
git fetch --prune --tags origin

set_step "refresh-upstream-main"
git checkout -B upstream-main upstream/main

set_step "rebase-feature-overlay"
if git show-ref --verify --quiet refs/heads/feature-overlay; then
  git checkout feature-overlay
else
  git checkout -B feature-overlay origin/feature-overlay
fi
git rebase upstream-main

set_step "reset-main-to-overlay"
if git show-ref --verify --quiet refs/heads/main; then
  git checkout main
else
  git checkout -B main origin/main
fi
git reset --hard feature-overlay

set_step "validate-build"
go build -o test-output ./cmd/server && rm -f test-output

if [[ "${DRY_RUN}" == "true" ]]; then
  set_step "dry-run-skip-publish"
  FAILED_STEP=""
  write_state_env
  echo "[sync-upstream-release] Dry run completed for ${UPSTREAM_TAG}"
  exit 0
fi

set_step "set-origin-auth-url"
: "${GITHUB_TOKEN:?GITHUB_TOKEN is required}"
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
git remote set-url origin "https://x-access-token:${GITHUB_TOKEN}@github.com/${GITHUB_REPOSITORY}.git"
export GH_TOKEN="${GH_TOKEN:-${GITHUB_TOKEN}}"

set_step "push-branches"
git push origin upstream-main --force-with-lease
git push origin feature-overlay --force-with-lease
git push origin main --force-with-lease

set_step "publish-tag"
git tag -fa "${UPSTREAM_TAG}" -m "Sync upstream release ${UPSTREAM_TAG}"
git push origin "refs/tags/${UPSTREAM_TAG}" --force

set_step "publish-release"
gh release create "${UPSTREAM_TAG}" --target main --title "${UPSTREAM_TAG}" --generate-notes

set_step "write-state-env"
FAILED_STEP=""
write_state_env

echo "[sync-upstream-release] Completed for ${UPSTREAM_TAG}"
