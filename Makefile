# Copyright 2026 The Agent Substrate Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# ko pushes to KO_DOCKER_REPO, e.g. localhost:5001 for kind. --base-import-paths
# names the image gsm-credential-provider, without a hash suffix.
KO       ?= go run github.com/google/ko@v0.19.1
KO_FLAGS ?= --base-import-paths

# VERSION is the release tag .ko.yaml stamps into main.version. It is exported
# even when empty, since ko fails on an unset VERSION, and so release-images and
# release-manifest build the same binary when run with the same value.
VERSION ?=
export VERSION

# Release builds drop KO_DEFAULTPLATFORMS, so one left set for kind cannot
# narrow a release below .ko.yaml's defaultPlatforms.
KO_RELEASE := env -u KO_DEFAULTPLATFORMS $(KO)

# The toolchain's own gofmt, not whichever one is first on PATH.
GOFMT := $(shell go env GOROOT)/bin/gofmt

.PHONY: all
all: verify test

.PHONY: build
build:
	go build ./...

.PHONY: test
test:
	go test -race ./...

.PHONY: fmt
fmt:
	$(GOFMT) -w .

.PHONY: verify
verify:
	@out="$$($(GOFMT) -l .)"; if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi
	go vet ./...
	go mod tidy -diff

# Builds the image and applies the manifests to the current kubectl context.
.PHONY: deploy
deploy:
	kubectl kustomize config | $(KO) apply $(KO_FLAGS) -f -

# Renders the manifests with the image pinned by digest, for a release. Run it
# with the same VERSION as release-images, so it pins the digest that was pushed.
.PHONY: release-manifest
release-manifest:
	kubectl kustomize config | $(KO_RELEASE) resolve $(KO_FLAGS) -f -

# Builds the image for VERSION, a release tag, and pushes it to KO_DOCKER_REPO
# tagged VERSION. Run it from a clean checkout of the tag.
.PHONY: release-images
release-images:
	@if [ -z "$(VERSION)" ] || [ -z "$(KO_DOCKER_REPO)" ]; then echo "VERSION and KO_DOCKER_REPO must be set"; exit 1; fi
	$(KO_RELEASE) build $(KO_FLAGS) --tags $(VERSION) ./cmd/gsm-credential-provider

.PHONY: undeploy
undeploy:
	kubectl delete -k config --ignore-not-found
