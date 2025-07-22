-include .env
export

# Makefile for development environment setup

TILT_VERSION      			?= 0.35.0
KIND_VERSION            	?= 0.29.0
CTLPTL_VERSION         		?= 0.8.42
CLUSTERCTL_VERSION       	?= 1.10.3
CAPONE_VERSION 			 	?= 0.1.7
KUBECTL_VERSION          	?= 1.31.4

TILT	  		:= $(SELF)/bin/tilt
KIND          	:= $(SELF)/bin/kind
CTLPTL         	:= $(SELF)/bin/ctlptl
CLUSTERCTL     	:= $(SELF)/bin/clusterctl
KUBECTL			:= $(SELF)/bin/kubectl

define CLUSTER_NODES_USERDATA
install -m u=rw,go=r -D /dev/fd/0 /etc/containerd/certs.d/$(LOCAL_REGISTRY)/hosts.toml <<EOF
[host."http://$(LOCAL_REGISTRY)"]
  capabilities = ["pull", "resolve"]
  skip_verify = true
EOF
systemctl restart containerd
cd ~ && curl -LO https://go.dev/dl/go1.24.0.linux-amd64.tar.gz && sudo rm -rf /usr/local/go && tar -C /usr/local -xzf go1.24.0.linux-amd64.tar.gz && echo "export PATH=$$PATH:/usr/local/go/bin" >> ~/.bashrc && source ~/.bashrc
apt update && apt install -y make
git clone https://github.com/kubernetes-csi/csi-test.git ~/csi-test
cd ~/csi-test/cmd/csi-sanity && export PATH=$$PATH:/usr/local/go/bin && export GOCACHE=/tmp/go-build-cache && make
endef

define CLUSTER_NODES_USERDATA_BASE64
$(file > /tmp/userdata.tmp,$(CLUSTER_NODES_USERDATA))$(shell base64 -w 0 /tmp/userdata.tmp && rm -f /tmp/userdata.tmp)
endef

define HELM_VALUES
CLUSTER_TEMPLATES:
  - templateName: "$(WORKLOAD_CLUSTER_NAME)-router"
    templateContent: |
      CONTEXT = [
        NETWORK = "YES",
        ONEAPP_VNF_DNS_ENABLED = "YES",
        ONEAPP_VNF_DNS_NAMESERVERS = "1.1.1.1,8.8.8.8",
        ONEAPP_VNF_DNS_USE_ROOTSERVERS = "NO",
        ONEAPP_VNF_NAT4_ENABLED = "YES",
        ONEAPP_VNF_NAT4_INTERFACES_OUT = "eth0",
        ONEAPP_VNF_ROUTER4_ENABLED = "YES",
        SSH_PUBLIC_KEY = "$$USER[SSH_PUBLIC_KEY]",
        TOKEN = "YES" ]
      CPU = "1"
      DISK = [
        IMAGE = "$(WORKLOAD_CLUSTER_NAME)-router" ]
      GRAPHICS = [
        LISTEN = "0.0.0.0",
        TYPE = "vnc" ]
      LXD_SECURITY_PRIVILEGED = "true"
      MEMORY = "512"
      NIC_DEFAULT = [
        MODEL = "virtio" ]
      OS = [
        ARCH = "x86_64",
        FIRMWARE_SECURE = "YES" ]
      VROUTER = "YES"
  - templateName: "$(WORKLOAD_CLUSTER_NAME)-master"
    templateContent: |
      CONTEXT = [
        START_SCRIPT_BASE64 = "$(CLUSTER_NODES_USERDATA_BASE64)",
        BACKEND = "YES",
        NETWORK = "YES",
        GROW_FS = "/",
        SET_HOSTNAME = "$$NAME",
        SSH_PUBLIC_KEY = "$$USER[SSH_PUBLIC_KEY]",
        TOKEN = "YES" ]
      CPU = "1"
      DISK = [
        IMAGE = "$(WORKLOAD_CLUSTER_NAME)-node",
        SIZE = "16384" ]
      GRAPHICS = [
        LISTEN = "0.0.0.0",
        TYPE = "vnc" ]
      HYPERVISOR = "kvm"
      LXD_SECURITY_PRIVILEGED = "true"
      MEMORY = "3072"
      OS = [
        ARCH = "x86_64",
        FIRMWARE_SECURE = "YES" ]
      SCHED_REQUIREMENTS = "HYPERVISOR=kvm"
      VCPU = "2"
  - templateName: "$(WORKLOAD_CLUSTER_NAME)-worker"
    templateContent: |
      CONTEXT = [
        START_SCRIPT_BASE64 = "$(CLUSTER_NODES_USERDATA_BASE64)",
        BACKEND = "YES",
        NETWORK = "YES",
        GROW_FS = "/",
        SET_HOSTNAME = "$$NAME",
        SSH_PUBLIC_KEY = "$$USER[SSH_PUBLIC_KEY]",
        TOKEN = "YES" ]
      CPU = "1"
      DISK = [
        IMAGE = "$(WORKLOAD_CLUSTER_NAME)-node",
        SIZE = "16384" ]
      GRAPHICS = [
        LISTEN = "0.0.0.0",
        TYPE = "vnc" ]
      HYPERVISOR = "kvm"
      LXD_SECURITY_PRIVILEGED = "true"
      MEMORY = "3072"
      OS = [
        ARCH = "x86_64",
        FIRMWARE_SECURE = "YES" ]
      SCHED_REQUIREMENTS = "HYPERVISOR=kvm"
      VCPU = "2"
endef


# NOTE: If using this insecure local registry,
# ensure you configure your container runtime to trust it.
# e.g. in docker: https://docs.docker.com/reference/cli/dockerd/#insecure-registries
# (e.g. add `"insecure-registries" : [ "172.20.0.1:5005" ]` in /etc/docker/daemon.json)
define CTLPTL_CLUSTER_YAML
---
apiVersion: ctlptl.dev/v1alpha1
kind: Registry
name: ctlptl-registry
port: 5005
listenAddress: 0.0.0.0
---
apiVersion: ctlptl.dev/v1alpha1
kind: Cluster
product: kind
registry: ctlptl-registry
kindV1Alpha4Cluster:
  nodes:
  - role: control-plane
    extraMounts:
      - hostPath: /var/run/docker.sock
        containerPath: /var/run/docker.sock
endef

WORKLOAD_CLUSTER_NAME ?= capone-workload
WORKLOAD_CLUSTER_KUBECONFIG ?= kubeconfig-workload.yaml

# Develop environment

.PHONY: mgmt-cluster-create mgmt-cluster-delete workload-cluster-deploy workload-cluster-destroy \
	workload-cluster-flannel workload-cluster-kubeconfig tilt-up tilt-down tilt-up-debug tilt-down-debug tilt-clean

mgmt-cluster-create: $(CLUSTERCTL) $(CTLPTL) $(KIND)
	@kind --version
	$(CTLPTL) apply -f- <<< "$$CTLPTL_CLUSTER_YAML"
	$(CLUSTERCTL) init --infrastructure=opennebula:v$(CAPONE_VERSION)

mgmt-cluster-destroy: $(CTLPTL)
	$(CTLPTL) delete -f- <<< "$$CTLPTL_CLUSTER_YAML"

workload-cluster-deploy: workload-cluster-init workload-cluster-flannel

workload-cluster-init: $(HELM)
	@if ! $(HELM) repo list -o json | jq -e '.[] | select(.name=="capone")' > /dev/null; then \
		$(HELM) repo add capone https://opennebula.github.io/cluster-api-provider-opennebula/charts/ && $(HELM) repo update; \
	fi
	@echo "$$HELM_VALUES" > /tmp/cluster_templates_values.yaml
	$(HELM) upgrade --install $(WORKLOAD_CLUSTER_NAME) capone/capone-kadm --version $(CAPONE_VERSION) \
		--set ONE_XMLRPC=$(ONE_XMLRPC) \
		--set ONE_AUTH=$(ONE_AUTH) \
		--values /tmp/cluster_templates_values.yaml
	@rm -f /tmp/cluster_templates_values.yaml

workload-cluster-flannel: $(CLUSTERCTL) $(KUBECTL)
	@for i in {1..60}; do \
		$(KUBECTL) --kubeconfig <($(CLUSTERCTL) get kubeconfig $(WORKLOAD_CLUSTER_NAME) 2>/dev/null) \
			get nodes >/dev/null 2>&1 && break; \
		echo "Waiting for workload cluster to be reachable... (attempt $$i/60)"; \
		sleep 5; \
	done
	@echo "Installing flannel..."
	@$(KUBECTL) --kubeconfig <($(CLUSTERCTL) get kubeconfig $(WORKLOAD_CLUSTER_NAME)) \
		apply -f https://raw.githubusercontent.com/flannel-io/flannel/master/Documentation/kube-flannel.yml

workload-cluster-kubeconfig: $(CLUSTERCTL)
	$(CLUSTERCTL) get kubeconfig $(WORKLOAD_CLUSTER_NAME) > $(WORKLOAD_CLUSTER_KUBECONFIG)

workload-cluster-destroy: $(HELM)
	$(HELM) uninstall $(WORKLOAD_CLUSTER_NAME)

tilt-up: $(TILT) $(CLUSTERCTL) $(KUBECTL) workload-cluster-kubeconfig
	export KUBECONFIG=$(WORKLOAD_CLUSTER_KUBECONFIG) && $(TILT) up --file $(SELF)/tilt/Tiltfile

tilt-down: $(TILT)
	$(TILT) down --file $(SELF)/tilt/Tiltfile

tilt-up-debug: $(TILT) $(CLUSTERCTL) $(KUBECTL) workload-cluster-kubeconfig
	export KUBECONFIG=$(WORKLOAD_CLUSTER_KUBECONFIG) && $(TILT) up --file $(SELF)/tilt/Tiltfile.debug

tilt-down-debug: $(TILT)
	$(TILT) down --file $(SELF)/tilt/Tiltfile.debug

tilt-clean:
	rm --preserve-root -rf $(SELF)/tilt/build

# Dependencies

.PHONY: tilt kind ctlptl clusterctl

tilt: $(TILT)
$(TILT):
	@[ -f $@-v${TILT_VERSION} ] || \
	{ curl -fsSL https://github.com/tilt-dev/tilt/releases/download/v${TILT_VERSION}/tilt.${TILT_VERSION}.linux.x86_64.tar.gz \
	| tar -xzO tilt | install -m u=rwx,go= -o $(USER) -D /dev/fd/0 $@-v$(TILT_VERSION); }
	@ln -sf $@-v${TILT_VERSION} $@

kind: $(KIND)
$(KIND):
	@[ -f $@-v$(KIND_VERSION) ] || \
	{ curl -fsSL https://github.com/kubernetes-sigs/kind/releases/download/v$(KIND_VERSION)/kind-linux-amd64 \
	| install -m u=rwx,go= -o $(USER) -D /dev/fd/0 $@-v$(KIND_VERSION); }
	@ln -sf $@-v$(KIND_VERSION) $@

ctlptl: $(CTLPTL)
$(CTLPTL):
	@[ -f $@-v$(CTLPTL_VERSION) ] || \
	{ curl -fsSL https://github.com/tilt-dev/ctlptl/releases/download/v$(CTLPTL_VERSION)/ctlptl.$(CTLPTL_VERSION).linux.x86_64.tar.gz \
	| tar -xzO -f- ctlptl \
	| install -m u=rwx,go= -o $(USER) -D /dev/fd/0 $@-v$(CTLPTL_VERSION); }
	@ln -sf $@-v$(CTLPTL_VERSION) $@

clusterctl: $(CLUSTERCTL)
$(CLUSTERCTL):
	@[ -f $@-v$(CLUSTERCTL_VERSION) ] || \
	{ curl -fsSL https://github.com/kubernetes-sigs/cluster-api/releases/download/v$(CLUSTERCTL_VERSION)/clusterctl-linux-amd64 \
	| install -m u=rwx,go= -o $(USER) -D /dev/fd/0 $@-v$(CLUSTERCTL_VERSION); }
	@ln -sf $@-v$(CLUSTERCTL_VERSION) $@