SELF := $(patsubst %/,%,$(dir $(abspath $(firstword $(MAKEFILE_LIST)))))
PATH := $(SELF)/bin:$(PATH)

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

CHARTS_DIR := $(SELF)/_charts
DEPLOY_DIR  := $(SELF)/_deploy

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN := $(shell go env GOPATH)/bin
else
GOBIN := $(shell go env GOBIN)
endif

GOLANGCI_LINT_VERSION		?= 2.2.1
HELM_VERSION				?= 3.17.3

SYSTEM_HELM := $(shell command -v helm 2>/dev/null)
GOLANGCI_LINT	:= $(SELF)/bin/golangci-lint
ifeq ($(SYSTEM_HELM),)
HELM := $(SELF)/bin/helm
else
HELM := $(SYSTEM_HELM)
endif

CLOSEST_TAG ?= $(shell git -C $(SELF) describe --tags --abbrev=0 2>/dev/null || echo v0.0.0)
BUILD_VERSION ?= $(shell git -C $(SELF) describe --tags --always --dirty 2>/dev/null || echo dev)
GIT_COMMIT ?= $(shell git -C $(SELF) rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
GO_LDFLAGS := -X github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/driver.driverVersion=$(BUILD_VERSION) -X github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/driver.driverCommit=$(GIT_COMMIT) -X github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/driver.driverBuildDate=$(BUILD_DATE)

# Local registry and tag used for building/pushing image targets
LOCAL_TAG ?= latest
LOCAL_REGISTRY ?= localhost:5005
# Registry to use for building/pushing image targets
REMOTE_REGISTRY ?= ghcr.io/sparkaiur
DOCKERHUB_REGISTRY ?= docker.io/nudevco

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Binaries to build
BUILD_BINS := opennebula-csi
# List of container image names to build and push
IMAGE_NAMES := opennebula-csi

-include .env
export LOCAL_REGISTRY LOCAL_TAG ONE_XMLRPC ONE_AUTH DEBUG_PORT WORKLOAD_CLUSTER_NAME WORKLOAD_CLUSTER_KUBECONFIG WORKER_NODES

include Makefile.dev.mk

.PHONY: all clean

all: build

clean: tilt-clean
	rm --preserve-root -rf '$(SELF)/bin/'
	rm --preserve-root -rf '$(DEPLOY_DIR)'
	rm --preserve-root -rf '$(CHARTS_DIR)'

# Development

.PHONY: lint lint-fix fmt vet test

lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run

lint-fix: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run --fix

fmt:
	go fmt ./...

vet:
	go vet ./...

test:
	go test ./... -v -count=1

# Build

.PHONY: build docker-build docker-push docker-release

build: fmt vet $(addprefix build-,$(BUILD_BINS))

build-%:
	go build -ldflags "$(GO_LDFLAGS)" -o $(SELF)/bin/$* ./cmd/$*/main.go

docker-build: $(addprefix docker-build-,$(IMAGE_NAMES))

docker-build-%:
	$(CONTAINER_TOOL) build \
		--build-arg VERSION=$(BUILD_VERSION) \
		--build-arg COMMIT=$(GIT_COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		-t $(LOCAL_REGISTRY)/$*:$(LOCAL_TAG) .

docker-push: $(addprefix docker-push-,$(IMAGE_NAMES))

docker-push-%:
	$(CONTAINER_TOOL) push $(LOCAL_REGISTRY)/$*:$(LOCAL_TAG)

# _PLATFORMS defines the target platforms for the manager image be built to provide support to multiple architectures.
# To use this option you need to:
# - be able to use docker buildx (https://docs.docker.com/reference/cli/docker/buildx/)
# - have enabled BuildKit (https://docs.docker.com/build/buildkit/)
# - be able to push the image to your registry
_PLATFORMS ?= linux/amd64,linux/arm64

docker-release: $(addprefix docker-release-,$(IMAGE_NAMES))

docker-release-%:
	-$(CONTAINER_TOOL) buildx create --name $*-builder
	$(CONTAINER_TOOL) buildx use $*-builder
	$(CONTAINER_TOOL) buildx build --push --platform=$(_PLATFORMS) \
		--build-arg VERSION=$(CLOSEST_TAG) \
		--build-arg COMMIT=$(GIT_COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		-t $(REMOTE_REGISTRY)/$*:$(CLOSEST_TAG) \
		-t $(REMOTE_REGISTRY)/$*:latest \
		-t $(DOCKERHUB_REGISTRY)/$*:$(CLOSEST_TAG) \
		-t $(DOCKERHUB_REGISTRY)/$*:latest \
		-f Dockerfile .
	-$(CONTAINER_TOOL) buildx rm $*-builder

# Helm

.PHONY: helm package helm-deploy helm-undeploy manifests manifests-dev

helm-package: $(HELM)
	install -m u=rwx,go=rx -d $(CHARTS_DIR)/$(CLOSEST_TAG)/opennebula-csi
	$(HELM) package helm/opennebula-csi/  \
	-d $(CHARTS_DIR)/$(CLOSEST_TAG)/opennebula-csi \
	--version $(subst v,,$(CLOSEST_TAG)) \
	--app-version $(CLOSEST_TAG)

helm-deploy: $(HELM) # Deploy OpenNebula CSI plugin using Helm to the cluster specified in ~/.kube/config.
	$(HELM) upgrade --install opennebula-csi helm/opennebula-csi
		--set image.repository=$(REMOTE_REGISTRY)/opennebula-csi \
		--set image.tag=$(CLOSEST_TAG) \
		--set image.pullPolicy="IfNotPresent" \
		--set oneApiEndpoint=$(ONE_XMLRPC) \
		--set credentials.inlineAuth=$(ONE_AUTH)

helm-undeploy: $(HELM) # Undeploy OpenNebula CSI plugin from the cluster specified in ~/.kube/config.
	$(HELM) uninstall opennebula-csi

manifests: $(HELM)
	$(HELM) template opennebula-csi helm/opennebula-csi \
		--set image.repository=$(REMOTE_REGISTRY)/opennebula-csi \
		--set image.tag=$(CLOSEST_TAG) \
		--set image.pullPolicy="IfNotPresent" \
		--set oneApiEndpoint=$(ONE_XMLRPC) \
		--set credentials.inlineAuth=$(ONE_AUTH) \
		| install -m u=rw,go=r -D /dev/fd/0 $(DEPLOY_DIR)/release/opennebula-csi.yaml

manifests-dev: $(HELM)
	$(HELM) template opennebula-csi helm/opennebula-csi \
		--set image.repository=$(LOCAL_REGISTRY)/opennebula-csi \
		--set image.tag=$(LOCAL_TAG) \
		--set image.pullPolicy="Always" \
		--set oneApiEndpoint=$(ONE_XMLRPC) \
		--set credentials.inlineAuth=$(ONE_AUTH) \
		| install -m u=rw,go=r -D /dev/fd/0 $(DEPLOY_DIR)/dev/opennebula-csi.yaml

# Dependencies

.PHONY: golangci-lint helm

golangci-lint: $(GOLANGCI_LINT)
$(GOLANGCI_LINT):
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,v$(GOLANGCI_LINT_VERSION))

ifeq ($(SYSTEM_HELM),)
helm: $(HELM)
$(HELM):
	@[ -f $@-v$(HELM_VERSION) ] || \
	{ curl -fsSL https://get.helm.sh/helm-v$(HELM_VERSION)-linux-amd64.tar.gz \
	| tar -xzO -f- linux-amd64/helm \
	| install -m u=rwx,go= -o $(USER) -D /dev/fd/0 $@-v$(HELM_VERSION); }
	@ln -sf $@-v$(HELM_VERSION) $@
else
helm:
	@printf 'Using system helm at %s\n' "$(HELM)"
endif

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3); \
echo "Downloading $${package}"; \
rm -f $(1) ||:; \
GOBIN=$(SELF)/bin go install $${package}; \
mv $(1) $(1)-$(3); \
}; \
ln -sf $(1)-$(3) $(1)
endef
