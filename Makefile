#
#Copyright 2024 NVIDIA
#
#Licensed under the Apache License, Version 2.0 (the "License");
#you may not use this file except in compliance with the License.
#You may obtain a copy of the License at
#
#    http://www.apache.org/licenses/LICENSE-2.0
#
#Unless required by applicable law or agreed to in writing, software
#distributed under the License is distributed on an "AS IS" BASIS,
#WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#See the License for the specific language governing permissions and
#limitations under the License.

ARCH ?= $(shell go env GOARCH)
OS ?= $(shell go env GOOS)
TAG ?=v25.7.1-rht
OVN_KUBERNETES_DIR ?= ovn-kubernetes
OVN_GITREF ?=
ifeq ($(OVN_GITREF),)
OVN_FROM := koji
else
OVN_FROM := source
OVN_GITSHA := $(shell git ls-remote "${OVN_REPO}" "${OVN_GITREF}" | sort -k2  -V  |tail -1 | awk '{ print $$1 }')
endif

GO_VERSION ?= 1.24
GO_IMAGE = quay.io/projectquay/golang:${GO_VERSION}

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

.PHONY: docker-build-ubuntu
docker-build-ubuntu:
	docker buildx build \
		--build-arg OVN_KUBERNETES_DIR=${OVN_KUBERNETES_DIR} \
		--build-arg BUILDER_IMAGE=${GO_IMAGE} \
		-t ovn-kube-ubuntu:${TAG} \
		--load \
		-f Dockerfile.ovn-kubernetes.ubuntu .

.PHONY: docker-build-fedora
docker-build-fedora:
	docker buildx build \
		--build-arg OVN_KUBERNETES_DIR=${OVN_KUBERNETES_DIR} \
		--build-arg BUILDER_IMAGE=${GO_IMAGE} \
		-t ovn-kube-fedora:${TAG}  \
		--load \
		-f Dockerfile.ovn-kubernetes.fedora .