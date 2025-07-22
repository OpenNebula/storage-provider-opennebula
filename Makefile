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
ENVSUBST_VERSION			?= 1.4.2
KUBECTL_VERSION				?= 1.31.4
KUSTOMIZE_VERSION			?= 5.6.0
HELM_VERSION				?= 3.17.3

GOLANGCI_LINT	:= $(SELF)/bin/golangci-lint
ENVSUBST  		:= $(SELF)/bin/envsubst
KUBECTL			:= $(SELF)/bin/kubectl
KUSTOMIZE		:= $(SELF)/bin/kustomize
HELM			:= $(SELF)/bin/helm

CLOSEST_TAG ?= $(shell git -C $(SELF) describe --tags --abbrev=0)

# Local registry and tag used for building/pushing image targets
LOCAL_TAG ?= latest
LOCAL_REGISTRY ?= localhost:5005
# Registry to use for building/pushing image targets
REMOTE_REGISTRY ?= ghcr.io/opennebula

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Binaries to build
BUILD_BINS := opennebula-cloud-controller-manager opennebula-csi-plugin
# List of container image names to build and push
IMAGE_NAMES := cloud-provider-opennebula opennebula-csi-plugin

-include .env
export

include Makefile.dev.mk

.PHONY: all clean

all: build

clean: tilt-clean
	rm --preserve-root -rf '$(SELF)/bin/'
	rm --preserve-root -rf '$(DEPLOY_DIR)'

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
	go build -o $(SELF)/bin/$* ./cmd/$*/main.go

docker-build: $(addprefix docker-build-,$(IMAGE_NAMES))

docker-build-%:
	$(CONTAINER_TOOL) build -t $(LOCAL_REGISTRY)/$*:$(LOCAL_TAG) .

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
	$(CONTAINER_TOOL) buildx build --push --platform=$(_PLATFORMS) -t $(REMOTE_REGISTRY)/$*:$(CLOSEST_TAG) -t $(REMOTE_REGISTRY)/$*:latest -f Dockerfile .
	-$(CONTAINER_TOOL) buildx rm $*-builder

# Helm

.PHONY: charts

define chart-generator-tool
charts: $(CHARTS_DIR)/$(CLOSEST_TAG)/$(1)-$(subst v,,$(CLOSEST_TAG)).tgz

$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1)-$(subst v,,$(CLOSEST_TAG)).tgz: $(CHARTS_DIR)/$(CLOSEST_TAG)/$(1) $(HELM)
	$(HELM) package -d $(CHARTS_DIR)/$(CLOSEST_TAG) $(CHARTS_DIR)/$(CLOSEST_TAG)/$(1)

$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1):              CCM_IMG := {{ tpl .Values.CCM_IMG . }}
$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1):              CCM_CTL := {{ .Values.CCM_CTL }}
$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1):         CLUSTER_NAME := {{ .Values.CLUSTER_NAME }}
$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1):        NODE_SELECTOR := {{ (toYaml .Values.nodeSelector) | nindent 8 }}
$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1):             ONE_AUTH := {{ .Values.ONE_AUTH }}
$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1):           ONE_XMLRPC := {{ .Values.ONE_XMLRPC }}
$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1): PRIVATE_NETWORK_NAME := {{ .Values.PRIVATE_NETWORK_NAME }}
$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1):  PUBLIC_NETWORK_NAME := {{ .Values.PUBLIC_NETWORK_NAME }}
$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1): ROUTER_TEMPLATE_NAME := {{ tpl .Values.ROUTER_TEMPLATE_NAME . }}

$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1): $(KUSTOMIZE) $(ENVSUBST)
	install -m u=rwx,go=rx -d $(CHARTS_DIR)/$(CLOSEST_TAG)/$(1)
	cp -rf helm/$(1) $(CHARTS_DIR)/$(CLOSEST_TAG)/.
	$(KUSTOMIZE) build kustomize/$(2) | $(ENVSUBST) \
	| install -m u=rw,go=r -D /dev/fd/0 $(CHARTS_DIR)/$(CLOSEST_TAG)/$(1)/templates/opennebula-cpi.yaml
endef

$(eval $(call chart-generator-tool,opennebula-cpi,base))

# Helm

.PHONY: charts

define chart-generator-tool
charts: $(CHARTS_DIR)/$(CLOSEST_TAG)/$(1)-$(subst v,,$(CLOSEST_TAG)).tgz

$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1)-$(subst v,,$(CLOSEST_TAG)).tgz: $(CHARTS_DIR)/$(CLOSEST_TAG)/$(1) $(HELM)
	$(HELM) package -d $(CHARTS_DIR)/$(CLOSEST_TAG) $(CHARTS_DIR)/$(CLOSEST_TAG)/$(1)

$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1):              CCM_IMG := {{ tpl .Values.CCM_IMG . }}
$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1):              CCM_CTL := {{ .Values.CCM_CTL }}
$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1):         CLUSTER_NAME := {{ .Values.CLUSTER_NAME }}
$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1):        NODE_SELECTOR := {{ (toYaml .Values.nodeSelector) | nindent 8 }}
$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1):             ONE_AUTH := {{ .Values.ONE_AUTH }}
$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1):           ONE_XMLRPC := {{ .Values.ONE_XMLRPC }}
$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1): PRIVATE_NETWORK_NAME := {{ .Values.PRIVATE_NETWORK_NAME }}
$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1):  PUBLIC_NETWORK_NAME := {{ .Values.PUBLIC_NETWORK_NAME }}
$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1): ROUTER_TEMPLATE_NAME := {{ tpl .Values.ROUTER_TEMPLATE_NAME . }}

$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1): $(KUSTOMIZE) $(ENVSUBST)
	install -m u=rwx,go=rx -d $(CHARTS_DIR)/$(CLOSEST_TAG)/$(1)
	cp -rf helm/$(1) $(CHARTS_DIR)/$(CLOSEST_TAG)/.
	$(KUSTOMIZE) build kustomize/$(2) | $(ENVSUBST) \
	| install -m u=rw,go=r -D /dev/fd/0 $(CHARTS_DIR)/$(CLOSEST_TAG)/$(1)/templates/opennebula-cpi.yaml
endef

$(eval $(call chart-generator-tool,opennebula-cpi,base))

# Deployment

ifndef ignore-not-found
ignore-not-found := false
endif

.PHONY: deploy-kadm undeploy-kadm deploy-rke2 undeploy-rke2

# Deploy controller to the K8s cluster specified in ~/.kube/config.
deploy-kadm deploy-rke2: deploy-%: $(KUSTOMIZE) $(ENVSUBST) $(KUBECTL)
	$(KUSTOMIZE) build kustomize/$*/ | $(ENVSUBST) | $(KUBECTL) apply -f-

# Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
undeploy-kadm undeploy-rke2: undeploy-%: $(KUSTOMIZE) $(ENVSUBST) $(KUBECTL)


# Helm

.PHONY: helm-deploy-opennebula-csi-plugin helm-undeploy-opennebula-csi-plugin manifests-opennebula-csi-plugin manifests-opennebula-csi-plugin-dev

helm-deploy-opennebula-csi-plugin: $(HELM) # Deploy OpenNebula CSI plugin using Helm to the cluster specified in ~/.kube/config.
	$(HELM) upgrade --install opennebula-csi-plugin helm/opennebula-csi-plugin
		--set image.repository=$(REMOTE_REGISTRY)/opennebula-csi-plugin \
		--set image.tag=$(CLOSEST_TAG) \
		--set image.pullPolicy="IfNotPresent" \
		--set oneApiEndpoint=$(ONE_XMLRPC) \
		--set oneAuth=$(ONE_AUTH)

helm-undeploy-opennebula-csi-plugin: $(HELM) # Undeploy OpenNebula CSI plugin from the cluster specified in ~/.kube/config.
	$(HELM) uninstall opennebula-csi-plugin

manifests-opennebula-csi-plugin: $(HELM)
	$(HELM) template opennebula-csi-plugin helm/opennebula-csi-plugin \
		--set image.repository=$(REMOTE_REGISTRY)/opennebula-csi-plugin \
		--set image.tag=$(CLOSEST_TAG) \
		--set image.pullPolicy="IfNotPresent" \
		--set oneApiEndpoint=$(ONE_XMLRPC) \
		--set oneAuth=$(ONE_AUTH) \
		| install -m u=rw,go=r -D /dev/fd/0 $(DEPLOY_DIR)/release/opennebula-csi-plugin.yaml

manifests-opennebula-csi-plugin-dev: $(HELM)
	$(HELM) template opennebula-csi-plugin helm/opennebula-csi-plugin \
		--set image.repository=$(LOCAL_REGISTRY)/opennebula-csi-plugin \
		--set image.tag=$(LOCAL_TAG) \
		--set image.pullPolicy="Always" \
		--set oneApiEndpoint=$(ONE_XMLRPC) \
		--set oneAuth=$(ONE_AUTH) \
		| install -m u=rw,go=r -D /dev/fd/0 $(DEPLOY_DIR)/dev/opennebula-csi-plugin.yaml


# Dependencies

.PHONY: golangci-lint envsubst kubectl kustomize helm

golangci-lint: $(GOLANGCI_LINT)
$(GOLANGCI_LINT):
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,v$(GOLANGCI_LINT_VERSION))

envsubst: $(ENVSUBST)
$(ENVSUBST):
	$(call go-install-tool,$(ENVSUBST),github.com/a8m/envsubst/cmd/envsubst,v$(ENVSUBST_VERSION))

helm: $(HELM)
$(HELM):
	@[ -f $@-v$(HELM_VERSION) ] || \
	{ curl -fsSL https://get.helm.sh/helm-v$(HELM_VERSION)-linux-amd64.tar.gz \
	| tar -xzO -f- linux-amd64/helm \
	| install -m u=rwx,go= -o $(USER) -D /dev/fd/0 $@-v$(HELM_VERSION); }
	@ln -sf $@-v$(HELM_VERSION) $@

kubectl: $(KUBECTL)
$(KUBECTL):
	@[ -f $@-v$(KUBECTL_VERSION) ] || \
	{ curl -fsSL https://dl.k8s.io/release/v$(KUBECTL_VERSION)/bin/linux/amd64/kubectl \
	| install -m u=rwx,go= -o $(USER) -D /dev/fd/0 $@-v$(KUBECTL_VERSION); }
	@ln -sf $@-v$(KUBECTL_VERSION) $@

kustomize: $(KUSTOMIZE)
$(KUSTOMIZE):
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,v$(KUSTOMIZE_VERSION))

helm: $(HELM)
$(HELM):
	@[ -f $@-v$(HELM_VERSION) ] || \
	{ curl -fsSL https://get.helm.sh/helm-v$(HELM_VERSION)-linux-amd64.tar.gz \
	| tar -xzO -f- linux-amd64/helm \
	| install -m u=rwx,go= -o $(USER) -D /dev/fd/0 $@-v$(HELM_VERSION); }
	@ln -sf $@-v$(HELM_VERSION) $@

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
