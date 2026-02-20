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

##@ Docker Build Targets

REGISTRY ?= example.com
OVNKUBERNETES_IMAGE ?= $(REGISTRY)/ovn-kubernetes-dpf
DPF_UTILS_IMAGE ?= $(REGISTRY)/ovn-kubernetes-dpf-utils
MULTIARCH_PLATFORMS ?= linux/amd64,linux/arm64
DOCKER_BUILDX_BUILDER ?= ovn-kubernetes-dpf-builder
DOCKER_BUILD_PROGRESS ?= plain
DOCKER_BUILD_PROVENANCE ?= false
DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
FULL_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
PROJECT_REPO ?= https://github.com/Mellanox/ovn-kubernetes-dpf

.PHONY: docker-buildx-setup
docker-buildx-setup: ## Ensure a buildx builder exists and is selected
	@if ! docker buildx inspect $(DOCKER_BUILDX_BUILDER) >/dev/null 2>&1; then \
		docker buildx create --name $(DOCKER_BUILDX_BUILDER) --driver docker-container --use >/dev/null; \
	else \
		docker buildx use $(DOCKER_BUILDX_BUILDER) >/dev/null 2>&1 || true; \
	fi

.PHONY: docker-build-ubuntu
docker-build-ubuntu:
	docker buildx build \
		--build-arg OVN_KUBERNETES_DIR=${OVN_KUBERNETES_DIR} \
		--build-arg BUILDER_IMAGE=${GO_IMAGE} \
		-t $(OVNKUBERNETES_IMAGE):$(TAG) \
		--load \
		-f Dockerfile.ovn-kubernetes.ubuntu .

.PHONY: docker-build-ubuntu-multiarch
docker-build-ubuntu-multiarch: docker-buildx-setup ## Build and push multi-arch Ubuntu image (amd64+arm64)
	docker buildx build \
		--platform $(MULTIARCH_PLATFORMS) \
		--provenance=$(DOCKER_BUILD_PROVENANCE) \
		--progress=$(DOCKER_BUILD_PROGRESS) \
		--label org.opencontainers.image.created=$(DATE) \
		--label org.opencontainers.image.revision=$(FULL_COMMIT) \
		--label org.opencontainers.image.source=$(PROJECT_REPO) \
		--label org.opencontainers.image.version=$(TAG) \
		--build-arg OVN_KUBERNETES_DIR=${OVN_KUBERNETES_DIR} \
		--build-arg BUILDER_IMAGE=${GO_IMAGE} \
		-t $(OVNKUBERNETES_IMAGE):$(TAG) \
		--push \
		-f Dockerfile.ovn-kubernetes.ubuntu .

.PHONY: docker-build-fedora
docker-build-fedora:
	docker buildx build \
		--build-arg OVN_KUBERNETES_DIR=${OVN_KUBERNETES_DIR} \
		--build-arg BUILDER_IMAGE=${GO_IMAGE} \
		-t $(OVNKUBERNETES_IMAGE):$(TAG)-fedora \
		--load \
		-f Dockerfile.ovn-kubernetes.fedora .

.PHONY: docker-build-fedora-multiarch
docker-build-fedora-multiarch: docker-buildx-setup ## Build and push multi-arch Fedora image (amd64+arm64)
	docker buildx build \
		--platform $(MULTIARCH_PLATFORMS) \
		--provenance=$(DOCKER_BUILD_PROVENANCE) \
		--progress=$(DOCKER_BUILD_PROGRESS) \
		--label org.opencontainers.image.created=$(DATE) \
		--label org.opencontainers.image.revision=$(FULL_COMMIT) \
		--label org.opencontainers.image.source=$(PROJECT_REPO) \
		--label org.opencontainers.image.version=$(TAG) \
		--build-arg OVN_KUBERNETES_DIR=${OVN_KUBERNETES_DIR} \
		--build-arg BUILDER_IMAGE=${GO_IMAGE} \
		-t $(OVNKUBERNETES_IMAGE):$(TAG)-fedora \
		--push \
		-f Dockerfile.ovn-kubernetes.fedora .

.PHONY: docker-build-dpf-utils
docker-build-dpf-utils: ## Build DPF utilities image
	docker buildx build \
		--build-arg builder_image=${GO_IMAGE} \
		-t $(DPF_UTILS_IMAGE):$(TAG) \
		--load \
		-f dpf-utils/Dockerfile \
		dpf-utils/

.PHONY: docker-build-dpf-utils-multiarch
docker-build-dpf-utils-multiarch: docker-buildx-setup ## Build and push multi-arch DPF utilities image (amd64+arm64)
	docker buildx build \
		--platform $(MULTIARCH_PLATFORMS) \
		--provenance=$(DOCKER_BUILD_PROVENANCE) \
		--progress=$(DOCKER_BUILD_PROGRESS) \
		--label org.opencontainers.image.created=$(DATE) \
		--label org.opencontainers.image.revision=$(FULL_COMMIT) \
		--label org.opencontainers.image.source=$(PROJECT_REPO) \
		--label org.opencontainers.image.version=$(TAG) \
		--build-arg builder_image=${GO_IMAGE} \
		-t $(DPF_UTILS_IMAGE):$(TAG) \
		--push \
		-f dpf-utils/Dockerfile \
		dpf-utils/

.PHONY: docker-build-multiarch
docker-build-multiarch: docker-build-ubuntu-multiarch docker-build-fedora-multiarch docker-build-dpf-utils-multiarch ## Build and push all multi-arch images

.PHONY: docker-push-ubuntu
docker-push-ubuntu: ## Push Ubuntu image to registry
	docker push $(OVNKUBERNETES_IMAGE):$(TAG)

.PHONY: docker-push-fedora
docker-push-fedora: ## Push Fedora image to registry
	docker push $(OVNKUBERNETES_IMAGE):$(TAG)-fedora

.PHONY: docker-push-dpf-utils
docker-push-dpf-utils: ## Push DPF utilities image to registry
	docker push $(DPF_UTILS_IMAGE):$(TAG)

##@ DPF Utils Targets

DPF_UTILS_DIR = dpf-utils

.PHONY: lint
lint: golangci-lint ## Run linter for DPF utilities
	cd $(DPF_UTILS_DIR) && $(GOLANGCI_LINT) run --timeout=5m ./...

.PHONY: test
test: ## Run tests for DPF utilities
	cd $(DPF_UTILS_DIR) && go test -v -coverprofile=coverage.out -covermode=atomic ./...

##@ Helm Chart Targets

HELM_CHART_DIR ?= helm/ovn-kubernetes-dpf
HELM_OUTPUT_DIR ?= _output/helm

.PHONY: helm-build
helm-build: yq
	@mkdir -p $(HELM_OUTPUT_DIR)
	@cp $(HELM_CHART_DIR)/values.yaml.tmpl $(HELM_CHART_DIR)/values.yaml
	@$(YQ) eval -i '.ovn-kubernetes-resource-injector.controllerManager.webhook.image.repository = "$(DPF_UTILS_IMAGE)"' $(HELM_CHART_DIR)/values.yaml
	@$(YQ) eval -i '.ovn-kubernetes-resource-injector.controllerManager.webhook.image.tag = "$(TAG)"' $(HELM_CHART_DIR)/values.yaml
	@$(YQ) eval -i '.nodeWithDPUManifests.image.repository = "$(OVNKUBERNETES_IMAGE)"' $(HELM_CHART_DIR)/values.yaml
	@$(YQ) eval -i '.nodeWithDPUManifests.image.tag = "$(TAG)"' $(HELM_CHART_DIR)/values.yaml
	@$(YQ) eval -i '.nodeWithoutDPUManifests.image.repository = "$(OVNKUBERNETES_IMAGE)"' $(HELM_CHART_DIR)/values.yaml
	@$(YQ) eval -i '.nodeWithoutDPUManifests.image.tag = "$(TAG)"' $(HELM_CHART_DIR)/values.yaml
	@$(YQ) eval -i '.dpuManifests.image.repository = "$(OVNKUBERNETES_IMAGE)"' $(HELM_CHART_DIR)/values.yaml
	@$(YQ) eval -i '.dpuManifests.image.tag = "$(TAG)"' $(HELM_CHART_DIR)/values.yaml
	@$(YQ) eval -i '.dpuManifests.imagedpf.repository = "$(DPF_UTILS_IMAGE)"' $(HELM_CHART_DIR)/values.yaml
	@$(YQ) eval -i '.dpuManifests.imagedpf.tag = "$(TAG)"' $(HELM_CHART_DIR)/values.yaml
	@$(YQ) eval -i '.controlPlaneManifests.image.repository = "$(OVNKUBERNETES_IMAGE)"' $(HELM_CHART_DIR)/values.yaml
	@$(YQ) eval -i '.controlPlaneManifests.image.tag = "$(TAG)"' $(HELM_CHART_DIR)/values.yaml
	@$(YQ) eval -i '.version = "$(TAG)"' $(HELM_CHART_DIR)/Chart.yaml
	@$(YQ) eval -i '.appVersion = "$(TAG)"' $(HELM_CHART_DIR)/Chart.yaml
	@helm package $(HELM_CHART_DIR) -d $(HELM_OUTPUT_DIR)
	@git checkout $(HELM_CHART_DIR)/values.yaml $(HELM_CHART_DIR)/Chart.yaml 2>/dev/null || true

.PHONY: helm-publish
helm-publish: helm-build ## Publish the Helm chart to OCI registry
	@helm push $(HELM_OUTPUT_DIR)/ovn-kubernetes-chart-$(TAG).tgz oci://$(REGISTRY)/charts

.PHONY: helm-clean
helm-clean:
	@rm -rf $(HELM_OUTPUT_DIR)
	@echo "Cleaned Helm build artifacts"

##@ Tool Dependencies

TOOLSDIR ?= $(CURDIR)/hack/tools/bin
YQ_VERSION ?= v4.45.1
export YQ ?= $(TOOLSDIR)/yq-$(YQ_VERSION)
GOLANGCI_LINT_VERSION ?= v1.62.2
export GOLANGCI_LINT ?= $(TOOLSDIR)/golangci-lint-$(GOLANGCI_LINT_VERSION)

define go-install-tool
@[ -f $(1) ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
GOBIN=$(TOOLSDIR) go install $${package} ;\
mv "$$(echo "$(1)" | sed "s/-$(3)$$//")" $(1) ;\
}
endef

$(TOOLSDIR):
	@mkdir -p $@

.PHONY: yq
yq: $(YQ) ## Download yq locally if necessary
$(YQ): | $(TOOLSDIR)
	$(call go-install-tool,$(YQ),github.com/mikefarah/yq/v4,$(YQ_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary
$(GOLANGCI_LINT): | $(TOOLSDIR)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))
