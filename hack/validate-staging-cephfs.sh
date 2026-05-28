#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VALUES_FILE="${VALUES_FILE:-examples/helm-values-cephfs-dynamic.yaml}"
NAMESPACE="${NAMESPACE:-kube-system}"
RELEASE_NAME="${RELEASE_NAME:-opennebula-csi}"
CSI_IMAGE_REPOSITORY="${CSI_IMAGE_REPOSITORY:-}"
CSI_IMAGE_TAG="${CSI_IMAGE_TAG:-}"

require() {
  local var_name="$1"
  if [[ -z "${!var_name:-}" ]]; then
    echo "missing required environment variable: ${var_name}" >&2
    exit 1
  fi
}

require KUBECONFIG
require ONE_XMLRPC
require ONE_AUTH
require CEPHFS_ADMIN_ID
require CEPHFS_ADMIN_KEY
require CEPHFS_NODE_USER_ID
require CEPHFS_NODE_USER_KEY

cd "${ROOT_DIR}"

go test ./...
helm lint helm/opennebula-csi --values "${VALUES_FILE}"
helm template opennebula-csi helm/opennebula-csi --values "${VALUES_FILE}" >/tmp/opennebula-csi-staging-render.yaml

kubectl get namespace "${NAMESPACE}" >/dev/null 2>&1 || kubectl create namespace "${NAMESPACE}"

kubectl -n "${NAMESPACE}" create secret generic opennebula-csi-auth \
  --from-literal=credentials="${ONE_AUTH}" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl -n "${NAMESPACE}" create secret generic cephfs-provisioner \
  --from-literal=adminID="${CEPHFS_ADMIN_ID}" \
  --from-literal=adminKey="${CEPHFS_ADMIN_KEY}" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl -n "${NAMESPACE}" create secret generic cephfs-node-stage \
  --from-literal=userID="${CEPHFS_NODE_USER_ID}" \
  --from-literal=userKey="${CEPHFS_NODE_USER_KEY}" \
  --dry-run=client -o yaml | kubectl apply -f -

helm_args=(
  upgrade --install "${RELEASE_NAME}" helm/opennebula-csi
  --namespace "${NAMESPACE}"
  --create-namespace
  --set "oneApiEndpoint=${ONE_XMLRPC}"
  --values "${VALUES_FILE}"
)

if [[ -n "${CSI_IMAGE_REPOSITORY}" ]]; then
  helm_args+=(--set "image.repository=${CSI_IMAGE_REPOSITORY}")
fi
if [[ -n "${CSI_IMAGE_TAG}" ]]; then
  helm_args+=(--set "image.tag=${CSI_IMAGE_TAG}")
fi

helm "${helm_args[@]}"

kubectl -n "${NAMESPACE}" rollout status statefulset/"${RELEASE_NAME}"-opennebula-csi-controller --timeout=5m
kubectl -n "${NAMESPACE}" rollout status daemonset/"${RELEASE_NAME}"-opennebula-csi-node --timeout=5m

kubectl apply -f examples/demo-busybox-cephfs-rwx.yaml
kubectl rollout status deployment/demo-cephfs-rwx --timeout=5m

echo "staging CephFS validation deployment is ready"
