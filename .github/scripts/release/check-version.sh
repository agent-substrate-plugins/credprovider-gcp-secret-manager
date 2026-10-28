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

# Fails unless VERSION is a release tag, vMAJOR.MINOR.PATCH or
# vMAJOR.MINOR.PATCH-PRERELEASE. The release workflow's tag filter cannot
# express the whole tag, so this runs before anything is built or published.
#
#   VERSION=<tag> .github/scripts/release/check-version.sh
#
# Used by the images jobs in .github/workflows/release.yaml.

set -o errexit -o nounset -o pipefail

if ! [[ "${VERSION}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "::error::${VERSION} is not a vMAJOR.MINOR.PATCH[-PRERELEASE] tag; nothing is published for it"
  exit 1
fi
