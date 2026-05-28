#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VALUES_FILE="${VALUES_FILE:-examples/helm-values-single-datastore.yaml}"
NAMESPACE="${NAMESPACE:-kube-system}"
RELEASE_NAME="${RELEASE_NAME:-opennebula-csi}"
CSI_IMAGE_REPOSITORY="${CSI_IMAGE_REPOSITORY:-}"
CSI_IMAGE_TAG="${CSI_IMAGE_TAG:-}"
LAB_KUBECONFIG="${LAB_KUBECONFIG:-${KUBECONFIG:-${ROOT_DIR}/refs/kubeconfig.yaml}}"
VALIDATION_NAMESPACE="${VALIDATION_NAMESPACE:-opennebula-csi-release-validation}"
SC_NAME="${SC_NAME:-opennebula-default-rwo}"
LOCAL_NODE="${LOCAL_NODE:-hplcsiw01}"
LOCAL_NODE_ALT="${LOCAL_NODE_ALT:-hplcsiw02}"
CNPG_TIMEOUT_SECONDS="${CNPG_TIMEOUT_SECONDS:-600}"
STICKY_TIMEOUT_SECONDS="${STICKY_TIMEOUT_SECONDS:-240}"

require() {
  local var_name="$1"
  if [[ -z "${!var_name:-}" ]]; then
    echo "missing required environment variable: ${var_name}" >&2
    exit 1
  fi
}

first_matching_name() {
  local kind="$1"
  shift
  local candidate
  for candidate in "$@"; do
    if kubectl -n "${NAMESPACE}" get "${kind}/${candidate}" >/dev/null 2>&1; then
      echo "${candidate}"
      return 0
    fi
  done
  return 1
}

wait_for_datastore_phase() {
  local datastore_name="$1"
  local expected_phase="$2"
  local timeout_seconds="${3:-120}"
  local deadline=$((SECONDS + timeout_seconds))
  while (( SECONDS < deadline )); do
    local current_phase
    current_phase="$(kubectl get opennebuladatastore "${datastore_name}" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    if [[ "${current_phase}" == "${expected_phase}" ]]; then
      return 0
    fi
    sleep 5
  done
  echo "timed out waiting for ${datastore_name} phase ${expected_phase}" >&2
  kubectl get opennebuladatastore "${datastore_name}" -o yaml >&2 || true
  return 1
}

cleanup() {
  kubectl --kubeconfig "${LAB_KUBECONFIG}" delete namespace "${VALIDATION_NAMESPACE}" --ignore-not-found=true --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT

configmap_has_key() {
  local namespace="$1"
  local name="$2"
  local key="$3"
  kubectl -n "${namespace}" get configmap "${name}" -o jsonpath="{.data.${key}}" 2>/dev/null | grep -q .
}

wait_for_configmap_key() {
  local namespace="$1"
  local name="$2"
  local key="$3"
  local expect_present="$4"
  local timeout_seconds="${5:-180}"
  local deadline=$((SECONDS + timeout_seconds))
  while (( SECONDS < deadline )); do
    if configmap_has_key "${namespace}" "${name}" "${key}"; then
      [[ "${expect_present}" == "present" ]] && return 0
    else
      [[ "${expect_present}" == "absent" ]] && return 0
    fi
    sleep 5
  done
  echo "timed out waiting for configmap ${namespace}/${name} key ${key} to become ${expect_present}" >&2
  kubectl -n "${namespace}" get configmap "${name}" -o yaml >&2 || true
  return 1
}

wait_for_event_reason() {
  local namespace="$1"
  local object_name="$2"
  local reason="$3"
  local timeout_seconds="${4:-180}"
  local deadline=$((SECONDS + timeout_seconds))
  while (( SECONDS < deadline )); do
    if kubectl -n "${namespace}" get events --field-selector "involvedObject.name=${object_name},reason=${reason}" --no-headers 2>/dev/null | grep -q .; then
      return 0
    fi
    sleep 5
  done
  echo "timed out waiting for event ${reason} on ${namespace}/${object_name}" >&2
  kubectl -n "${namespace}" get events --field-selector "involvedObject.name=${object_name}" >&2 || true
  return 1
}

require ONE_XMLRPC
require ONE_AUTH

cd "${ROOT_DIR}"

export KUBECONFIG="${LAB_KUBECONFIG}"

go test ./...
helm lint helm/opennebula-csi --values "${VALUES_FILE}"
helm template "${RELEASE_NAME}" helm/opennebula-csi \
  --values "${VALUES_FILE}" \
  --set "oneApiEndpoint=${ONE_XMLRPC}" \
  --set "inventoryController.enabled=true" >/tmp/opennebula-csi-release-validation-render.yaml

kubectl get namespace "${NAMESPACE}" >/dev/null 2>&1 || kubectl create namespace "${NAMESPACE}"
kubectl delete namespace "${VALIDATION_NAMESPACE}" --ignore-not-found=true --wait=true >/dev/null 2>&1 || true
kubectl create namespace "${VALIDATION_NAMESPACE}" >/dev/null 2>&1 || true

helm_args=(
  upgrade --install "${RELEASE_NAME}" helm/opennebula-csi
  --namespace "${NAMESPACE}"
  --create-namespace
  --force-conflicts
  --take-ownership
  --set "oneApiEndpoint=${ONE_XMLRPC}"
  --set-string "credentials.inlineAuth=${ONE_AUTH}"
  --set "inventoryController.enabled=true"
  --values "${VALUES_FILE}"
)

if [[ -n "${CSI_IMAGE_REPOSITORY}" ]]; then
  helm_args+=(--set "image.repository=${CSI_IMAGE_REPOSITORY}")
fi
if [[ -n "${CSI_IMAGE_TAG}" ]]; then
  helm_args+=(--set "image.tag=${CSI_IMAGE_TAG}" --set "image.pullPolicy=Always")
fi

helm "${helm_args[@]}"

controller_name="$(first_matching_name statefulset "${RELEASE_NAME}-controller" "${RELEASE_NAME}-opennebula-csi-controller")"
node_name="$(first_matching_name daemonset "${RELEASE_NAME}-node" "${RELEASE_NAME}-opennebula-csi-node")"
inventory_name="$(first_matching_name deployment "${RELEASE_NAME}-inventory" "${RELEASE_NAME}-opennebula-csi-inventory-controller")"
webhook_service_name="$(first_matching_name service "${RELEASE_NAME}-last-node-webhook" "${RELEASE_NAME}-opennebula-csi-last-node-webhook")"
webhook_config_name="$(kubectl get mutatingwebhookconfiguration -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | grep -E "^(${RELEASE_NAME}-last-node-preference|${RELEASE_NAME}-opennebula-csi-last-node-preference)$" | head -1)"

kubectl -n "${NAMESPACE}" rollout restart statefulset/"${controller_name}" || true
kubectl -n "${NAMESPACE}" rollout restart daemonset/"${node_name}" || true
kubectl -n "${NAMESPACE}" rollout restart deployment/"${inventory_name}" || true
kubectl -n "${NAMESPACE}" delete pod "${controller_name}-0" --ignore-not-found=true --wait=false || true

kubectl -n "${NAMESPACE}" rollout status statefulset/"${controller_name}" --timeout=10m
kubectl -n "${NAMESPACE}" rollout status daemonset/"${node_name}" --timeout=10m
kubectl -n "${NAMESPACE}" rollout status deployment/"${inventory_name}" --timeout=10m
kubectl -n "${NAMESPACE}" get service "${webhook_service_name}" >/dev/null
kubectl get mutatingwebhookconfiguration "${webhook_config_name}" >/dev/null

node_pod="$(kubectl -n "${NAMESPACE}" get pods -l app.kubernetes.io/component=node -o wide --no-headers | awk -v node="${LOCAL_NODE}" '$7==node {print $1; exit}')"
if [[ -z "${node_pod}" ]]; then
  echo "failed to resolve node daemonset pod on ${LOCAL_NODE}" >&2
  exit 1
fi

kubectl wait --for=condition=Established --timeout=2m crd/opennebuladatastores.storageprovider.opennebula.sparkaiur.io
kubectl wait --for=condition=Established --timeout=2m crd/opennebulanodes.storageprovider.opennebula.sparkaiur.io
ds_output="$(kubectl get opennebuladatastores)"
node_output="$(kubectl get opennebulanodes)"
echo "${ds_output}"
echo "${node_output}"
echo "${ds_output}" | head -1 | grep -Eq 'STATUS|Status'
echo "${ds_output}" | head -1 | grep -Eq 'ID'
echo "${ds_output}" | head -1 | grep -Eq 'NAME|Name'
echo "${ds_output}" | head -1 | grep -Eq 'CAPACITY|Capacity'
echo "${ds_output}" | head -1 | grep -Eq 'METRICS|Metrics'
echo "${node_output}" | head -1 | grep -Eq 'DISPLAY|Display'
echo "${node_output}" | head -1 | grep -Eq 'SYSTEMDS|SystemDS'
test -n "$(kubectl get opennebuladatastores -o jsonpath='{.items[0].metadata.name}')"
test -n "$(kubectl get opennebulanodes -o jsonpath='{.items[0].metadata.name}')"

kubectl get opennebuladatastores -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.maintenanceMode}{"\t"}{.spec.maintenanceMessage}{"\n"}{end}' \
  | awk '$2=="true" {print $1}' \
  | while read -r stale_ds; do
      [[ -n "${stale_ds}" ]] || continue
      kubectl patch opennebuladatastore "${stale_ds}" --type merge -p '{"spec":{"maintenanceMode":false,"maintenanceMessage":""}}'
      wait_for_datastore_phase "${stale_ds}" "Enabled" 120
    done

sc_datastore_name="$(kubectl get opennebuladatastores -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.storageClassesDisplay}{"\n"}{end}' | awk -F'\t' -v sc="${SC_NAME}" '$2 ~ "(^|,)" sc "($|,)" {print $1; exit}')"
maint_ds="$(kubectl get opennebuladatastores -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.phase}{"\t"}{.status.backend}{"\n"}{end}' | awk -v exclude="${sc_datastore_name}" '$2=="Enabled" && $3=="local" && $1!=exclude {print $1; exit}')"
if [[ -z "${maint_ds}" ]]; then
  maint_ds="$(kubectl get opennebuladatastores -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.phase}{"\t"}{.status.backend}{"\n"}{end}' | awk '$2=="Enabled" && $3=="local" {print $1; exit}')"
fi
if [[ -n "${maint_ds}" ]]; then
  maint_sc_name="$(kubectl get opennebuladatastore "${maint_ds}" -o jsonpath='{.status.storageClassesDisplay}' | cut -d',' -f1)"
  if [[ -z "${maint_sc_name}" || "${maint_sc_name}" == "-" ]]; then
    echo "no StorageClass found for maintenance validation datastore ${maint_ds}" >&2
    exit 1
  fi
  kubectl patch opennebuladatastore "${maint_ds}" --type merge -p '{"spec":{"maintenanceMode":true,"maintenanceMessage":"release validation maintenance mode"}}'
  wait_for_datastore_phase "${maint_ds}" "Disabled" 120
  kubectl apply -n "${VALIDATION_NAMESPACE}" -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: maintenance-block-test
spec:
  accessModes: ["ReadWriteOnce"]
  resources:
    requests:
      storage: 1Gi
  storageClassName: ${maint_sc_name}
EOF
  sleep 20
  phase="$(kubectl -n "${VALIDATION_NAMESPACE}" get pvc maintenance-block-test -o jsonpath='{.status.phase}')"
  if [[ "${phase}" == "Bound" ]]; then
    echo "maintenance mode did not block new provisioning" >&2
    exit 1
  fi
  kubectl delete -n "${VALIDATION_NAMESPACE}" pvc maintenance-block-test --ignore-not-found=true --wait=false
  kubectl patch opennebuladatastore "${maint_ds}" --type merge -p '{"spec":{"maintenanceMode":false,"maintenanceMessage":""}}'
  wait_for_datastore_phase "${maint_ds}" "Enabled" 120
fi

kubectl apply -n "${VALIDATION_NAMESPACE}" -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: release-local-a
spec:
  accessModes: ["ReadWriteOnce"]
  resources:
    requests:
      storage: 1Gi
  storageClassName: ${SC_NAME}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: release-local-b
spec:
  accessModes: ["ReadWriteOnce"]
  resources:
    requests:
      storage: 1Gi
  storageClassName: ${SC_NAME}
---
apiVersion: v1
kind: Pod
metadata:
  name: release-local-smoke
spec:
  nodeName: ${LOCAL_NODE}
  restartPolicy: Never
  containers:
    - name: busybox
      image: busybox:1.36
      command: ["sh", "-c", "echo alpha >/data-a/check && echo beta >/data-b/check && sleep 300"]
      volumeMounts:
        - name: data-a
          mountPath: /data-a
        - name: data-b
          mountPath: /data-b
  volumes:
    - name: data-a
      persistentVolumeClaim:
        claimName: release-local-a
    - name: data-b
      persistentVolumeClaim:
        claimName: release-local-b
EOF

kubectl -n "${VALIDATION_NAMESPACE}" wait --for=condition=Ready pod/release-local-smoke --timeout=10m
kubectl -n "${VALIDATION_NAMESPACE}" exec release-local-smoke -- sh -c 'test "$(cat /data-a/check)" = "alpha" && test "$(cat /data-b/check)" = "beta"'
kubectl -n "${NAMESPACE}" logs "${node_pod}" -c opennebula-csi | grep -Eq 'resolvedBy.*(by-id|cache|alias|exact)'

validate_ds_id="$(kubectl get opennebuladatastores -o jsonpath='{range .items[*]}{.status.id}{"\t"}{.status.phase}{"\t"}{.status.backend}{"\n"}{end}' | awk '$2=="Enabled" && $3=="local" {print $1; exit}')"
if [[ -n "${validate_ds_id}" ]]; then
  go run ./cmd/opennebula-csi \
    --mode=inventory-validate \
    --datastore-id="${validate_ds_id}" \
    --storage-class="${SC_NAME}" \
    --size=1Gi >/tmp/inventory-validate.json
  grep -q '"phase": "Succeeded"' /tmp/inventory-validate.json
  test "$(kubectl get opennebuladatastore "ds-${validate_ds_id}" -o jsonpath='{.status.metricsDisplay}')" != "-"
fi

kubectl -n "${VALIDATION_NAMESPACE}" patch pvc release-local-a --type merge -p '{"spec":{"resources":{"requests":{"storage":"2Gi"}}}}'
kubectl -n "${VALIDATION_NAMESPACE}" wait --for=jsonpath='{.status.capacity.storage}'=2Gi pvc/release-local-a --timeout=10m || true
inplace_size_bytes="$(kubectl -n "${VALIDATION_NAMESPACE}" exec release-local-smoke -- sh -c "df -B1 /data-a | tail -1 | awk '{print \\$2}'")"
if [[ "${inplace_size_bytes}" -lt 1800000000 ]]; then
  echo "in-place expanded filesystem size too small while pod is running: ${inplace_size_bytes}" >&2
  exit 1
fi
non_opt_in_pv="$(kubectl -n "${VALIDATION_NAMESPACE}" get pvc release-local-a -o jsonpath='{.spec.volumeName}')"
non_opt_in_handle="$(kubectl get pv "${non_opt_in_pv}" -o jsonpath='{.spec.csi.volumeHandle}')"
kubectl -n "${VALIDATION_NAMESPACE}" delete pod release-local-smoke --wait=true
sleep 10
wait_for_configmap_key "${NAMESPACE}" "opennebula-csi-sticky-attachment-state" "${non_opt_in_handle}" "absent" 30
kubectl -n "${VALIDATION_NAMESPACE}" apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: release-local-smoke
spec:
  nodeName: ${LOCAL_NODE}
  restartPolicy: Never
  containers:
    - name: busybox
      image: busybox:1.36
      command: ["sh", "-c", "df -B1 /data-a | tail -1 | awk '{print \$2}' >/tmp/size && cat /tmp/size && sleep 300"]
      volumeMounts:
        - name: data-a
          mountPath: /data-a
        - name: data-b
          mountPath: /data-b
  volumes:
    - name: data-a
      persistentVolumeClaim:
        claimName: release-local-a
    - name: data-b
      persistentVolumeClaim:
        claimName: release-local-b
EOF
kubectl -n "${VALIDATION_NAMESPACE}" wait --for=condition=Ready pod/release-local-smoke --timeout=10m
post_restart_size_bytes="$(kubectl -n "${VALIDATION_NAMESPACE}" exec release-local-smoke -- cat /tmp/size)"
if [[ "${post_restart_size_bytes}" -lt 1800000000 ]]; then
  echo "post-restart expanded filesystem size too small: ${post_restart_size_bytes}" >&2
  exit 1
fi

sticky_start_epoch="$(date +%s)"
kubectl apply -n "${VALIDATION_NAMESPACE}" -f - <<EOF
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: sticky-local-restart
spec:
  serviceName: sticky-local-restart
  replicas: 1
  selector:
    matchLabels:
      app: sticky-local-restart
  template:
    metadata:
      labels:
        app: sticky-local-restart
    spec:
      nodeName: ${LOCAL_NODE}
      terminationGracePeriodSeconds: 5
      containers:
        - name: busybox
          image: busybox:1.36
          command: ["sh", "-c", "echo sticky >/data/check && sleep 300"]
          volumeMounts:
            - name: data
              mountPath: /data
  volumeClaimTemplates:
    - metadata:
        name: data
        annotations:
          storage-provider.opennebula.sparkaiur.io/restart-optimization: "sticky-local-restart-v1"
          storage-provider.opennebula.sparkaiur.io/detach-grace-seconds: "90"
      spec:
        accessModes:
          - ReadWriteOnce
        storageClassName: ${SC_NAME}
        resources:
          requests:
            storage: 1Gi
EOF
kubectl -n "${VALIDATION_NAMESPACE}" rollout status statefulset/sticky-local-restart --timeout=10m
sticky_pvc="data-sticky-local-restart-0"
sticky_pv="$(kubectl -n "${VALIDATION_NAMESPACE}" get pvc "${sticky_pvc}" -o jsonpath='{.spec.volumeName}')"
sticky_handle="$(kubectl get pv "${sticky_pv}" -o jsonpath='{.spec.csi.volumeHandle}')"
sticky_pod="sticky-local-restart-0"
kubectl -n "${VALIDATION_NAMESPACE}" scale statefulset sticky-local-restart --replicas=0
kubectl -n "${VALIDATION_NAMESPACE}" rollout status statefulset/sticky-local-restart --timeout=10m
wait_for_configmap_key "${NAMESPACE}" "opennebula-csi-sticky-attachment-state" "${sticky_handle}" "present" 120
wait_for_event_reason "${VALIDATION_NAMESPACE}" "${sticky_pvc}" "DetachGraceStarted" 120
kubectl -n "${VALIDATION_NAMESPACE}" scale statefulset sticky-local-restart --replicas=1
kubectl -n "${VALIDATION_NAMESPACE}" rollout status statefulset/sticky-local-restart --timeout="${STICKY_TIMEOUT_SECONDS}s"
kubectl -n "${VALIDATION_NAMESPACE}" wait --for=condition=Ready pod/"${sticky_pod}" --timeout="${STICKY_TIMEOUT_SECONDS}s"
wait_for_configmap_key "${NAMESPACE}" "opennebula-csi-sticky-attachment-state" "${sticky_handle}" "absent" 120
wait_for_event_reason "${VALIDATION_NAMESPACE}" "${sticky_pvc}" "DetachGraceReused" 120
sticky_ready_epoch="$(date +%s)"
echo "sticky_same_node_restart_seconds=$((sticky_ready_epoch-sticky_start_epoch))"
last_attached_node="$(kubectl get pv "${sticky_pv}" -o jsonpath='{.metadata.annotations.storage-provider\.opennebula\.sparkaiur\.io/last-attached-node}')"
[[ "${last_attached_node}" == "${LOCAL_NODE}" ]]

kubectl -n "${VALIDATION_NAMESPACE}" scale statefulset sticky-local-restart --replicas=0
kubectl -n "${VALIDATION_NAMESPACE}" rollout status statefulset/sticky-local-restart --timeout=10m
wait_for_configmap_key "${NAMESPACE}" "opennebula-csi-sticky-attachment-state" "${sticky_handle}" "present" 120
wait_for_event_reason "${VALIDATION_NAMESPACE}" "${sticky_pvc}" "DetachGraceStarted" 120
wait_for_configmap_key "${NAMESPACE}" "opennebula-csi-sticky-attachment-state" "${sticky_handle}" "absent" 180
wait_for_event_reason "${VALIDATION_NAMESPACE}" "${sticky_pvc}" "DetachGraceExpired" 180

kubectl apply -n "${VALIDATION_NAMESPACE}" -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: last-node-preference-probe
spec:
  restartPolicy: Never
  containers:
    - name: busybox
      image: busybox:1.36
      command: ["sh", "-c", "sleep 300"]
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: ${sticky_pvc}
EOF
preferred_last_node="$(kubectl -n "${VALIDATION_NAMESPACE}" get pod last-node-preference-probe -o jsonpath='{.metadata.annotations.storage-provider\.opennebula\.sparkaiur\.io/preferred-last-node}')"
[[ "${preferred_last_node}" == "${LOCAL_NODE}" ]]
kubectl -n "${VALIDATION_NAMESPACE}" get pod last-node-preference-probe -o yaml | grep -q 'preferredDuringSchedulingIgnoredDuringExecution'
kubectl -n "${VALIDATION_NAMESPACE}" delete pod last-node-preference-probe --wait=true

kubectl -n "${VALIDATION_NAMESPACE}" apply -f - <<EOF
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: sticky-local-move
spec:
  serviceName: sticky-local-move
  replicas: 1
  selector:
    matchLabels:
      app: sticky-local-move
  template:
    metadata:
      labels:
        app: sticky-local-move
    spec:
      nodeName: ${LOCAL_NODE}
      terminationGracePeriodSeconds: 5
      containers:
        - name: busybox
          image: busybox:1.36
          command: ["sh", "-c", "echo move >/data/check && sleep 300"]
          volumeMounts:
            - name: data
              mountPath: /data
  volumeClaimTemplates:
    - metadata:
        name: data
        annotations:
          storage-provider.opennebula.sparkaiur.io/restart-optimization: "sticky-local-restart-v1"
          storage-provider.opennebula.sparkaiur.io/detach-grace-seconds: "90"
      spec:
        accessModes:
          - ReadWriteOnce
        storageClassName: ${SC_NAME}
        resources:
          requests:
            storage: 1Gi
EOF
kubectl -n "${VALIDATION_NAMESPACE}" rollout status statefulset/sticky-local-move --timeout=10m
move_pvc="data-sticky-local-move-0"
move_pv="$(kubectl -n "${VALIDATION_NAMESPACE}" get pvc "${move_pvc}" -o jsonpath='{.spec.volumeName}')"
move_handle="$(kubectl get pv "${move_pv}" -o jsonpath='{.spec.csi.volumeHandle}')"
kubectl -n "${VALIDATION_NAMESPACE}" scale statefulset sticky-local-move --replicas=0
kubectl -n "${VALIDATION_NAMESPACE}" rollout status statefulset/sticky-local-move --timeout=10m
wait_for_configmap_key "${NAMESPACE}" "opennebula-csi-sticky-attachment-state" "${move_handle}" "present" 120
wait_for_event_reason "${VALIDATION_NAMESPACE}" "${move_pvc}" "DetachGraceStarted" 120
kubectl -n "${VALIDATION_NAMESPACE}" patch statefulset sticky-local-move --type merge -p "{\"spec\":{\"template\":{\"spec\":{\"nodeName\":\"${LOCAL_NODE_ALT}\"}}}}"
kubectl -n "${VALIDATION_NAMESPACE}" scale statefulset sticky-local-move --replicas=1
kubectl -n "${VALIDATION_NAMESPACE}" rollout status statefulset/sticky-local-move --timeout=10m
kubectl -n "${VALIDATION_NAMESPACE}" wait --for=condition=Ready pod/sticky-local-move-0 --timeout=10m
wait_for_configmap_key "${NAMESPACE}" "opennebula-csi-sticky-attachment-state" "${move_handle}" "absent" 180
wait_for_event_reason "${VALIDATION_NAMESPACE}" "${move_pvc}" "DetachGraceCancelled" 180
move_last_node="$(kubectl get pv "${move_pv}" -o jsonpath='{.metadata.annotations.storage-provider\.opennebula\.sparkaiur\.io/last-attached-node}')"
[[ "${move_last_node}" == "${LOCAL_NODE_ALT}" ]]

start_epoch="$(date +%s)"
kubectl apply -n "${VALIDATION_NAMESPACE}" -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: cnpg-bootstrap-pvc
spec:
  accessModes: ["ReadWriteOnce"]
  resources:
    requests:
      storage: 1Gi
  storageClassName: ${SC_NAME}
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: cnpg-bootstrap-smoke
spec:
  serviceName: cnpg-bootstrap-smoke
  replicas: 1
  selector:
    matchLabels:
      app: cnpg-bootstrap-smoke
  template:
    metadata:
      labels:
        app: cnpg-bootstrap-smoke
    spec:
      nodeName: ${LOCAL_NODE}
      initContainers:
        - name: bootstrap
          image: busybox:1.36
          command: ["sh", "-c", "date >/var/lib/postgresql/data/bootstrap.txt"]
          volumeMounts:
            - name: data
              mountPath: /var/lib/postgresql/data
      containers:
        - name: postgres
          image: busybox:1.36
          command: ["sh", "-c", "test -s /var/lib/postgresql/data/bootstrap.txt && echo ready >/tmp/ready && sleep 300"]
          volumeMounts:
            - name: data
              mountPath: /var/lib/postgresql/data
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: cnpg-bootstrap-pvc
EOF
kubectl -n "${VALIDATION_NAMESPACE}" rollout status statefulset/cnpg-bootstrap-smoke --timeout="${CNPG_TIMEOUT_SECONDS}s"
ready_epoch="$(date +%s)"
echo "cnpg_bootstrap_seconds=$((ready_epoch-start_epoch))"
kubectl -n "${VALIDATION_NAMESPACE}" exec statefulset/cnpg-bootstrap-smoke -- test -s /var/lib/postgresql/data/bootstrap.txt

go run ./cmd/opennebula-csi --mode=support-bundle >/tmp/opennebula-csi-support-bundle.json
grep -q '"datastores"' /tmp/opennebula-csi-support-bundle.json
grep -q '"nodes"' /tmp/opennebula-csi-support-bundle.json
grep -q '"benchmarkRuns"' /tmp/opennebula-csi-support-bundle.json
grep -q '"stickyAttachments"' /tmp/opennebula-csi-support-bundle.json
grep -q '"hotplugQueue"' /tmp/opennebula-csi-support-bundle.json
grep -q '"adaptiveTimeouts"' /tmp/opennebula-csi-support-bundle.json
grep -q '"adaptiveRecommendations"' /tmp/opennebula-csi-support-bundle.json

temp_sc_ds="$(kubectl get opennebuladatastores -o jsonpath='{range .items[*]}{.status.id}{"\t"}{.status.backend}{"\n"}{end}' | awk '$2=="local" {print $1; exit}')"
if [[ -n "${temp_sc_ds}" ]]; then
  kubectl apply -f - <<EOF
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: opennebula-local-immediate-validation
provisioner: csi.opennebula.io
volumeBindingMode: Immediate
allowVolumeExpansion: false
parameters:
  datastoreIDs: "${temp_sc_ds}"
EOF
  if go run ./cmd/opennebula-csi --mode=preflight --output=json >/tmp/preflight.json 2>/dev/null; then
    true
  fi
  grep -q 'local datastore should use WaitForFirstConsumer\|Immediate binding' /tmp/preflight.json
  kubectl delete storageclass opennebula-local-immediate-validation --ignore-not-found=true
fi

echo "release lab validation passed"
