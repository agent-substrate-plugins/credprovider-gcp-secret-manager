#!/usr/bin/env bash

# Copyright 2026 The Agent Substrate Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Creates a DRAFT GitHub release for an existing tag, with notes generated
# from .github/release.yml plus a committer list. A maintainer edits and
# publishes the draft. Adapted from agent-substrate/substrate's
# hack/release/draft-release.sh.
#
#   hack/release/draft-release.sh [--dry-run] <tag>
#
# --dry-run prints the notes and creates nothing. <tag> need not exist: the
# notes then run up to the local HEAD commit, which must already be in the
# repository (pushed to a branch there). If a release for <tag> already
# exists, the script prints the notes and leaves the release alone, so
# rerunning it never overwrites edited notes.
#
# Needs git, jq, and an authenticated gh with write access to the repository
# (the generate-notes API requires it even for --dry-run). GITHUB_REPOSITORY
# selects the repository; it defaults to
# agent-substrate-plugins/credprovider-gcp-secret-manager.

set -o errexit -o nounset -o pipefail

ROOT="$(git rev-parse --show-toplevel)"
UPSTREAM_REPO='agent-substrate-plugins/credprovider-gcp-secret-manager'
REPO="${GITHUB_REPOSITORY:-${UPSTREAM_REPO}}"
SEMVER='^v[0-9]+\.[0-9]+\.[0-9]+$'
SEMVER_PRE='^v[0-9]+\.[0-9]+\.[0-9]+-[0-9A-Za-z.-]+$'
# Where the upstream release workflow publishes the image (KO_DOCKER_REPO in
# .github/workflows/release.yaml, plus the image name ko derives with
# --base-import-paths).
RELEASE_IMAGE='ghcr.io/agent-substrate-plugins/credprovider-gcp-secret-manager/gsm-credential-provider'
# The release manifest the workflow attaches to the draft.
RELEASE_MANIFEST='gsm-credential-provider.yaml'
# Keep in step with the Install prerequisites in README.md.
MIN_SUBSTRATE="newer than v0.6.0, whose egress gateway calls the \`substrate.credprovider.v1\` API"

usage() {
  echo "usage: $0 [--dry-run] <tag>" >&2
  exit 2
}

errfile="$(mktemp)"
trap 'rm -f "${errfile}"' EXIT

# api <step> <gh api args...> prints the response. On failure it prints the
# step and gh's error, and returns 1; gh's error stays in ${errfile}.
api() {
  local step="$1"
  shift
  if ! gh api "$@" 2>"${errfile}"; then
    echo "error: ${step}: $(<"${errfile}")" >&2
    return 1
  fi
}

# exists <api-path> returns 0 if the resource exists and 1 if the API answers
# 404, or 422 as the commits endpoint does for an unknown SHA. Any other
# failure (gh not logged in, rate limit) stops the script with gh's error, so
# it is not mistaken for "not found".
exists() {
  if gh api "$1" --silent 2>"${errfile}"; then
    return 0
  fi
  if grep -qE 'HTTP (404|422)' "${errfile}"; then
    return 1
  fi
  echo "error: checking $1: $(<"${errfile}")" >&2
  exit 1
}

dry_run=false
if [[ "${1:-}" == --dry-run ]]; then
  dry_run=true
  shift
fi
[[ $# -eq 1 ]] || usage
tag="$1"

prerelease=false
if [[ "${tag}" =~ ${SEMVER_PRE} ]]; then
  prerelease=true
elif ! [[ "${tag}" =~ ${SEMVER} ]]; then
  echo "error: ${tag} is not a vMAJOR.MINOR.PATCH[-PRERELEASE] tag" >&2
  exit 1
fi

# target is the ref the notes run up to: the tag, or HEAD for a dry run of a
# tag that does not exist yet.
target="${tag}"
if ! exists "repos/${REPO}/git/ref/tags/${tag}"; then
  if [[ "${dry_run}" != true ]]; then
    echo "error: tag ${tag} does not exist in ${REPO}; push it first," \
      "or use --dry-run to preview the notes up to HEAD" >&2
    exit 1
  fi
  target="$(git rev-parse HEAD)"
  if ! exists "repos/${REPO}/commits/${target}"; then
    echo "error: HEAD (${target}) is not in ${REPO}; push it to a branch there," \
      "or set GITHUB_REPOSITORY to a repository that has it" >&2
    exit 1
  fi
fi

# The notes start at the newest final release below this tag's version. A
# final release therefore diffs against the previous final release, not its
# own release candidates, and every rc of a version shares one starting point.
# Tags come from the API because a local clone may lack them. As in substrate,
# the first release's notes start at v0.0.0, on main's first commit. The
# history imported from substrate is a second root, so it is in the range.
base="${tag%%-*}"
tags="$(api "listing tags" "repos/${REPO}/tags" --paginate --jq '.[].name')"
# With no tags at all (a fork, for example), the notes would silently cover
# the whole history. GitHub needs the previous tag in ${REPO} itself.
if ! grep -qE "${SEMVER}" <<<"${tags}"; then
  echo "error: ${REPO} has no vX.Y.Z tags, so there is no previous release" \
    "to start the notes from. Push the release tags to it first; before the" \
    "first release, v0.0.0 on \$(git rev-list --max-parents=0 --first-parent HEAD)" >&2
  exit 1
fi
previous="$(
  {
    grep -E "${SEMVER}" <<<"${tags}" | grep -vxF "${base}" || true
    echo "${base}"
  } | sort -V | grep -B1 -xF "${base}" | grep -vxF "${base}" || true
)"

generate_args=(-f "tag_name=${tag}")
if [[ "${target}" != "${tag}" ]]; then
  generate_args+=(-f "target_commitish=${target}")
fi
if [[ -n "${previous}" ]]; then
  generate_args+=(-f "previous_tag_name=${previous}")
fi
if ! notes="$(api "generating notes" "repos/${REPO}/releases/generate-notes" \
  "${generate_args[@]}" --jq .body)"; then
  if grep -q 'HTTP 404' "${errfile}"; then
    echo "GitHub answers 404 here when the token cannot write to ${REPO}." \
      "Generating notes needs write access, even for --dry-run." >&2
  fi
  exit 1
fi

# Only the upstream release workflow publishes the image and attaches the
# manifest (see .github/workflows/release.yaml), so only its notes say where
# they are.
if [[ "${REPO}" == "${UPSTREAM_REPO}" ]]; then
  notes+=$'\n\n## Image\n\n'
  notes+="The image for this release is published as \`${RELEASE_IMAGE}:${tag}\`"
  notes+=" for \`linux/amd64\` and \`linux/arm64\`."
  notes+=" The attached \`${RELEASE_MANIFEST}\` deploys it pinned by digest."
  notes+=$'\n\n## Install\n\n'
  notes+="Requires substrate ${MIN_SUBSTRATE}. Follow [Install](https://github.com/${REPO}/blob/${tag}/README.md#install),"
  notes+=" deploying the release manifest in place of \`make deploy\`. Before applying it:"
  notes+=$'\n\n'
  notes+="- Apply the project policy, [\`config/project-policy.yaml\`](https://github.com/${REPO}/blob/${tag}/config/project-policy.yaml)"
  notes+=" edited for your atespaces. The manifest leaves it out, and the provider does not start without it."$'\n'
  notes+="- To act as a Google service account, set the \`iam.gke.io/gcp-service-account\` annotation on the"
  notes+=" \`gsm-credential-provider\` ServiceAccount in the manifest."
  notes+=$'\n\n```sh\n'
  notes+="curl -fsSLO https://github.com/${REPO}/releases/download/${tag}/${RELEASE_MANIFEST}"$'\n'
  notes+="curl -fsSLO https://github.com/${REPO}/releases/download/${tag}/${RELEASE_MANIFEST}.sha256"$'\n'
  notes+="shasum -a 256 -c ${RELEASE_MANIFEST}.sha256"$'\n'
  notes+="kubectl apply -f ${RELEASE_MANIFEST}"$'\n'
  notes+='```'
  notes+=$'\n\n'"Then point substrate's egress gateway at the provider, as in step 4 of Install."
fi

# GitHub only lists first-time contributors; list everyone. get-contributors.sh
# reads the range from the local clone, so both ends must be present locally
# (the release workflow checks out full history).
if [[ -n "${previous}" ]]; then
  owner="${REPO%%/*}"
  name="${REPO#*/}"
  contributors="$(ORG="${owner}" REPO="${name}" \
    "${ROOT}/hack/util/get-contributors.sh" "${previous}..${target}" | tail -n +2)"
  if [[ -n "${contributors}" ]]; then
    notes+=$'\n\n## Committers in this release\n\n'
    notes+="${contributors}"
  fi
fi

up_to=""
if [[ "${target}" != "${tag}" ]]; then
  up_to=", up to HEAD ${target}"
fi
echo "Release notes for ${tag} (previous release: ${previous:-none}${up_to}):"
echo
echo "${notes}"
echo

if [[ "${dry_run}" == true ]]; then
  echo "Dry run: no release created."
  exit 0
fi
if gh release view "${tag}" --repo "${REPO}" >/dev/null 2>&1; then
  echo "A release for ${tag} already exists; left unchanged."
  exit 0
fi

create_args=(--repo "${REPO}" --draft --verify-tag --title "${tag}" --notes-file -)
if [[ "${prerelease}" == true ]]; then
  create_args+=(--prerelease)
fi
gh release create "${tag}" "${create_args[@]}" <<<"${notes}"
