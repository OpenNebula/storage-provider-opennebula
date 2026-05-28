# opennebula-csi Helm Chart

This chart deploys the SparkAI OpenNebula CSI driver into Kubernetes.

It installs:

- the CSI controller StatefulSet
- the CSI node DaemonSet
- service accounts and RBAC
- optional metrics Services and ServiceMonitors
- optional preflight validation Job
- optional StorageClass objects

Images published for this chart:

- `ghcr.io/sparkaiur/opennebula-csi:<tag>`
- `docker.io/nudevco/opennebula-csi:<tag>`

Chart repo:

- `https://sparkaiur.github.io/storage-provider-opennebula/charts/`

## What This Chart Supports

- OpenNebula disk-backed PVC provisioning on explicitly configured datastores
- datastore selection policies: `least-used`, `ordered`, `autopilot`
- local, Ceph RBD, and SparkAI CephFS datastore backends
- `ReadWriteOnce`, `ReadOnlyMany`, and CephFS-backed filesystem volumes for `ReadWriteOnce`, `ReadOnlyMany`, and `ReadWriteMany`
- CSI resize, metrics, preflight checks, snapshots, and clone workflows
- stable detached-disk expansion and dynamic CephFS expansion
- gated alpha features for CephFS snapshot/clone, CephFS self-healing, and topology accessibility

## Prerequisites

- Kubernetes cluster with CSI sidecar compatibility for the versions used by this chart
- OpenNebula API endpoint reachable from controller and node pods
- OpenNebula credentials available either in an existing Secret or inline through Helm values
- OpenNebula datastores prepared for the backends you intend to use
- for CephFS:
  - a `FILE` datastore with the SparkAI CephFS attributes
  - Ceph monitors reachable from the pods
  - provisioner and node-stage secrets available in Kubernetes

## Install

Add the chart repo:

```bash
helm repo add sparkai-opennebula https://sparkaiur.github.io/storage-provider-opennebula/charts/
helm repo update
```

### Install with an Existing Secret

Create the auth secret:

```bash
kubectl -n kube-system create secret generic opennebula-csi-auth \
  --from-literal=credentials='oneadmin:changeme'
```

Install the chart:

```bash
helm upgrade --install opennebula-csi sparkai-opennebula/opennebula-csi \
  --namespace kube-system \
  --create-namespace \
  --set credentials.existingSecret.name=opennebula-csi-auth
```

### Install with Inline Credentials

```bash
helm upgrade --install opennebula-csi sparkai-opennebula/opennebula-csi \
  --namespace kube-system \
  --create-namespace \
  --set oneApiEndpoint=http://opennebula.example.com:2633/RPC2 \
  --set credentials.inlineAuth='oneadmin:changeme'
```

Inline auth is supported for convenience. An existing Secret is the preferred production path.

## Typical Configuration Patterns

### Single Datastore

```yaml
driver:
  defaultDatastores:
    - "111"

storageClasses:
  - name: opennebula-csi
    reclaimPolicy: Delete
    allowVolumeExpansion: true
    volumeBindingMode: WaitForFirstConsumer
    parameters:
      datastoreIDs: "111"
      fsType: "xfs"
      cache: "none"
      driver: "raw"
```

For local-style datastores (`local`, `fs`, `fs_lvm`, `fs_lvm_ssh`), prefer `WaitForFirstConsumer`. It lets Kubernetes choose a node first, which gives the driver and OpenNebula the best chance to select a compatible placement. Treat these classes as node-sensitive RWO storage, not portable storage.

### Multiple Datastores with Fallback

```yaml
driver:
  defaultDatastores:
    - "100"
    - "101"
    - "102"
  datastoreSelectionPolicy: least-used
```

### CephFS Filesystem StorageClass

```yaml
driver:
  defaultDatastores:
    - "300"
  allowedDatastoreTypes:
    - local
    - ceph
    - cephfs

storageClasses:
  - name: opennebula-cephfs
    reclaimPolicy: Delete
    allowVolumeExpansion: true
    volumeBindingMode: Immediate
    parameters:
      datastoreIDs: "300"
      csi.storage.k8s.io/provisioner-secret-name: cephfs-provisioner
      csi.storage.k8s.io/provisioner-secret-namespace: kube-system
      csi.storage.k8s.io/controller-expand-secret-name: cephfs-provisioner
      csi.storage.k8s.io/controller-expand-secret-namespace: kube-system
      csi.storage.k8s.io/node-stage-secret-name: cephfs-node-stage
      csi.storage.k8s.io/node-stage-secret-namespace: kube-system
```

CephFS-backed filesystem volumes can now be provisioned for single-node and multi-node filesystem access:

- `ReadWriteOnce`
- `ReadOnlyMany`
- `ReadWriteMany`

The driver uses the CephFS shared-filesystem path when the StorageClass resolves exclusively to CephFS datastores or explicitly uses `sharedFilesystemPath` / `sharedFilesystemSubvolumeGroup`. Do not mix CephFS and image-backed datastore IDs in the same StorageClass.

Recommended secret split for dynamic CephFS:

- `cephfs-provisioner`
  - used by the controller for `ceph fs subvolume create`, `rm`, `getpath`, and later resize/snapshot/clone flows
  - should use a stronger Ceph identity with monitor, MDS, OSD, and `mgr` caps sufficient for CephFS subvolume lifecycle
- `cephfs-node-stage`
  - used by the node plugin for `ceph-fuse` mounts and the opt-in kernel CephFS mount path
  - should use a narrower mount-oriented Ceph identity

For the staging lab, the working split is:

- provisioner user: `client.opennebula-csi`
- node-stage user: `client.opennebula-csi-node`

If dynamic CephFS fails with an error like:

```text
does your client key have mgr caps?
```

the provisioner secret is using a Ceph user without sufficient `mgr` privileges for subvolume lifecycle commands.

The controller sidecar also needs a longer CSI RPC timeout than the default for long-running create, attach, detach, and resize flows. The chart now renders:

```text
csi-provisioner --timeout=960s
```

Without that timeout increase, the driver may successfully complete the backend work but the external sidecar can still report `DeadlineExceeded`.

For CephFS expansion, add the standard CSI controller-expand secret refs to the StorageClass. In practice this is usually the same secret as the provisioner path:

```yaml
parameters:
  csi.storage.k8s.io/controller-expand-secret-name: cephfs-provisioner
  csi.storage.k8s.io/controller-expand-secret-namespace: kube-system
```

For static CephFS filesystem volumes, `sharedFilesystemPath` must already exist in the filesystem. The driver will mount that path, but it does not create the directory for you.

`cephfsMounter` defaults to `fuse`. Set `cephfsMounter: kernel` only when `featureGates.cephfsKernelMounts=true` and the host kernel already exposes CephFS client support. On Omni/Talos that support must come from the node image or system extensions on every worker that may mount the volume.

### Alpha Feature Gates

```yaml
featureGates:
  cephfsSnapshots: true
  cephfsClones: true
  cephfsSelfHealing: true
  cephfsPersistentRecovery: true
  cephfsKernelMounts: true
  topologyAccessibility: true
```

When `topologyAccessibility=true`, label nodes with:

```text
topology.opennebula.sparkaiur.io/system-ds=<opennebula-system-datastore-id>
```

For local-backed StorageClasses:

- prefer `volumeBindingMode: WaitForFirstConsumer`
- keep `featureGates.compatibilityAwareSelection=true`
- do not assume the driver will live-migrate local PVC data between nodes
- the controller uses size-aware hotplug timeouts, allows only one active VM hotplug per node, and returns retryable `Aborted` when another same-node hotplug is already in progress
- node-side device discovery uses the same per-volume timeout budget that the controller computed during publish
- if a VM stays non-ready through the full timeout, the driver puts that VM into a temporary hotplug cooldown and rejects further hotplug work with retryable `Unavailable`
- recreating MinIO tenants with local-backed PVCs should still be treated as node-sticky; use Ceph RBD or CephFS if the workload must remain portable across nodes
- use Ceph RBD for portable attached-disk RWO and CephFS for portable filesystem RWO or RWX

### Restart-optimized local StatefulSets

For MinIO-like StatefulSets on local datastores, the chart can enable a best-effort same-node restart fast path. It is enabled at the driver level by default, but only activates for PVCs that opt in with annotations.

Opt-in example:

```yaml
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: minio
spec:
  serviceName: minio
  replicas: 3
  selector:
    matchLabels:
      app: minio
  template:
    metadata:
      labels:
        app: minio
    spec:
      containers:
        - name: minio
          image: quay.io/minio/minio:latest
          args: ["server", "/data"]
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
        storageClassName: opennebula-local-rwo
        resources:
          requests:
            storage: 8Gi
```

Behavior:

- on `ControllerUnpublishVolume`, the driver keeps the local disk attached for a short grace period instead of detaching immediately
- if the replacement pod is scheduled back to the same node during that grace window, the driver reuses the existing attachment and skips detach/reattach
- if the pod lands on a different node, the grace is cancelled and the normal detach/attach path proceeds immediately
- stale same-node detaches are treated as successful when Kubernetes still desires the PVC on that node
- expired sticky state is cleared without detach when OpenNebula already reports the old-node attachment absent

### ConfigMap-driven maintenance mode

For rolling node maintenance, set `maintainenceMode=true` or `maintenanceMode=true` in `opennebula-csi-hotplug-state` in the driver namespace. The controller will prepare eligible local non-CephFS `ReadWriteOnce` volumes, publish `maintainenceReady=true`, inject required same-node affinity for eligible pods, hold maintenance detaches without physical OpenNebula detach, and reject cross-node attach attempts with retryable `Unavailable`.

When maintenance ends, set the mode key to `false`. The controller removes both ready keys and releases maintenance holds gradually using `driver.maintenanceMode.releaseMinSeconds` and `driver.maintenanceMode.releaseMaxSeconds`.

This is best-effort same-node reuse only. The CSI driver does not pin scheduling to the previous node.

### OpenNebula metadata drift guard

For local non-CephFS `ReadWriteOnce` volumes, the controller checks OpenNebula image ownership and VM disk records before cross-node attach. If the image still reports a different VM owner, or detach fails while ownership metadata remains, the driver returns `FailedPrecondition` with actionable owner VM, image, disk, and target details instead of issuing another blind hotplug request.

Repeated matching failures are persisted in `opennebula-csi-volume-quarantine-state` for a temporary quarantine window. The guard is read-only: it never edits OpenNebula DB state and never runs host-level repair commands.

### Local Device Recovery

When an OpenNebula VM reports a disk attached but the node plugin cannot discover the device inside the guest, the node records a missing-device report in `opennebula-csi-node-device-state`. The controller leader watches those reports and, after the configured threshold, performs a same-node recovery for eligible local non-CephFS `ReadWriteOnce` volumes: detach from the reporting node, attach back to that same node, confirm the OpenNebula attachment, and clear the report once the node can retry staging.

This recovery path never moves a volume to a different node. It skips CephFS, RWX, non-local backends, missing desired state, NotReady Kubernetes nodes, and non-running OpenNebula VMs. Failed recovery attempts are rate-limited with `driver.localDeviceRecovery.cooldownSeconds` and capped with `driver.localDeviceRecovery.maxAttemptsPerVolume`.

`v0.4.7` adds four supporting behaviors on top of that restart fast path:

- node-side device discovery prefers `/dev/disk/by-id` with a cache-backed serial lookup
- same-node hotplug work is queued fairly instead of failing fast
- a mutating webhook adds a soft last-node preference to eligible local single-writer pods
- a conservative attachment reconciler repairs stale OpenNebula and `VolumeAttachment` drift

To opt out of the soft last-node preference for a specific Pod or PVC, set:

```yaml
metadata:
  annotations:
    storage-provider.opennebula.sparkaiur.io/last-node-preference: "disabled"
```

## Values Reference

`Required` meanings:

- `No`: safe default exists
- `Yes`: must be set
- `Conditional`: required only when the related feature is used

### Global Values

| Parameter | Description | Default | Required |
| --- | --- | --- | --- |
| `nameOverride` | Override the chart name portion used for generated resource names. | `""` | No |
| `fullnameOverride` | Override the full generated release name. | `""` | No |
| `namespaceOverride` | Override the namespace used by chart resources. | `""` | No |
| `oneApiEndpoint` | OpenNebula XML-RPC endpoint exposed to the driver as `ONE_XMLRPC`. | `"http://localhost:2633/RPC2"` | Conditional |
| `imagePullSecrets` | Image pull secrets added to controller, node, and preflight pods. | `[]` | No |
| `debugPort` | Optional debug port exposed on the driver containers. | `null` | No |

### Credentials

| Parameter | Description | Default | Required |
| --- | --- | --- | --- |
| `credentials.existingSecret.name` | Name of an existing Secret containing OpenNebula credentials. | `""` | Conditional |
| `credentials.existingSecret.key` | Secret key inside `credentials.existingSecret.name` used for `ONE_AUTH`. | `"credentials"` | No |
| `credentials.inlineAuth` | Inline OpenNebula credentials used to create a Secret from the chart. | `""` | Conditional |

One of `credentials.existingSecret.name` or `credentials.inlineAuth` must be set.

### Image

| Parameter | Description | Default | Required |
| --- | --- | --- | --- |
| `image.repository` | Driver image repository used by controller, node, and default preflight image selection. | `"nudevco/opennebula-csi"` | No |
| `image.tag` | Driver image tag. | `"v0.5.15"` | No |
| `image.pullPolicy` | Image pull policy for the driver image. | `"IfNotPresent"` | No |

### Driver

| Parameter | Description | Default | Required |
| --- | --- | --- | --- |
| `driver.logLevel` | Kubernetes `-v` log level passed to the driver binary. | `5` | No |
| `driver.defaultDatastores` | Default list of OpenNebula datastore IDs or aliases used when StorageClasses do not override `datastoreIDs`. | `[]` | Conditional |
| `driver.datastoreSelectionPolicy` | Default datastore selection policy. Supported values: `least-used`, `ordered`, `autopilot`. | `"least-used"` | No |
| `driver.vmHotplugTimeoutSeconds` | Legacy alias for the base VM hotplug timeout. | `120` | No |
| `driver.vmHotplugTimeoutBaseSeconds` | Base VM hotplug timeout before size scaling is applied. | `120` | No |
| `driver.vmHotplugTimeoutPer100GiSeconds` | Additional timeout added for each 100 GiB bucket of actual disk size. | `60` | No |
| `driver.vmHotplugTimeoutMaxSeconds` | Maximum timeout cap for a single VM hotplug operation. | `900` | No |
| `driver.vmHotplugStuckVmCooldownSeconds` | Cooldown period applied after a VM stays stuck in hotplug through the full timeout. | `300` | No |
| `driver.nodeDeviceDiscoveryTimeoutSeconds` | Dedicated node-side device discovery timeout. This stays shorter than the controller hotplug budget so healthy fast-path retries happen quickly. | `30` | No |
| `driver.nodeExpand.verifyTimeoutSeconds` | Maximum time for node-side resize convergence before returning `DeadlineExceeded`. | `120` | No |
| `driver.nodeExpand.retryIntervalSeconds` | Retry interval for node-side checks between device visibility, growfs execution, and filesystem size validation. | `2` | No |
| `driver.nodeExpand.sizeToleranceBytes` | Allowed slack between requested and observed filesystem size to account for filesystem metadata overhead. | `134217728` | No |
| `driver.nodeDeviceCache.enabled` | Enable node-local device cache and stable serial/by-id resolution. | `true` | No |
| `driver.nodeDeviceCache.ttlSeconds` | Cache TTL for confirmed device paths. | `600` | No |
| `driver.nodeDeviceCache.udevSettleTimeoutSeconds` | Timeout for `udevadm settle` before device rescan on miss. | `10` | No |
| `driver.nodeDeviceCache.rescanOnMissEnabled` | Allow SCSI host rescan when device resolution misses. | `true` | No |
| `driver.hotplugQueue.enabled` | Queue same-node hotplug work instead of failing fast on lock contention. | `true` | No |
| `driver.hotplugQueue.maxWaitSeconds` | Maximum time a queued hotplug request waits before failing. | `180` | No |
| `driver.hotplugQueue.ageBoostSeconds` | Time after which older queued work is promoted one priority class. | `30` | No |
| `driver.hotplugQueue.dedupeEnabled` | Coalesce duplicate queued hotplug requests for the same node, operation, and volume. | `true` | No |
| `driver.hotplugQueue.perItemWaitSeconds` | Extra wait budget added per active or queued request ahead of a new request. | `60` | No |
| `driver.hotplugQueue.maxWaitCapSeconds` | Upper bound for dynamic queue wait budgets. | `900` | No |
| `driver.hotplugQueue.maxActiveSeconds` | Maximum execution time for an active queued hotplug request before it is classified as timed out. | `900` | No |
| `driver.hotplugQueue.snapshotDebounceSeconds` | Debounce interval for hotplug queue ConfigMap snapshots during recovery churn. Empty queue snapshots still flush immediately. | `2` | No |
| `driver.hotplugDiagnostics.enabled` | Persist read-only OpenNebula HOTPLUG observations for support bundles, inventory status, and timeout diagnosis. | `true` | No |
| `driver.hotplugDiagnostics.stuckAfterSeconds` | HOTPLUG age after which an unchanged observation is classified as stuck. | `300` | No |
| `driver.hotplugDiagnostics.progressWindowSeconds` | Required unchanged observation window before a HOTPLUG VM is classified as stuck. | `60` | No |
| `driver.hotplugDiagnostics.recoveryMode` | Reserved recovery mode selector. Current implementation is read-only and never mutates OpenNebula state. | `"readOnly"` | No |
| `driver.nodeHotplugGuard.enabled` | Enable node-health-aware hotplug pause after repeated unhealthy-node hotplug failures. | `true` | No |
| `driver.nodeHotplugGuard.failureThreshold` | Unhealthy-node hotplug failures allowed before pausing all hotplug work for that node. | `2` | No |
| `driver.nodeHotplugGuard.requireKubernetesReady` | Require Kubernetes `NodeReady=True` before clearing a node hotplug pause. | `true` | No |
| `driver.nodeHotplugGuard.requireOpenNebulaReady` | Require the OpenNebula VM to be `RUNNING` before clearing a node hotplug pause. | `true` | No |
| `driver.localRestartOptimization.enabled` | Enable best-effort same-node restart reuse for opted-in local PVCs. | `true` | No |
| `driver.localRestartOptimization.detachGraceSeconds` | Default delayed-detach grace used for opted-in local PVCs. | `90` | No |
| `driver.localRestartOptimization.maxDetachGraceSeconds` | Upper bound for per-PVC delayed-detach overrides. | `300` | No |
| `driver.localRestartOptimization.requireNodeReady` | Require Kubernetes node and OpenNebula VM readiness before starting delayed detach. | `true` | No |
| `driver.maintenanceMode.releaseMinSeconds` | Minimum delay used when releasing ConfigMap-driven maintenance attachment holds after mode exit. | `300` | No |
| `driver.maintenanceMode.releaseMaxSeconds` | Maximum stagger delay used when releasing ConfigMap-driven maintenance attachment holds after mode exit. | `1800` | No |
| `driver.metadataDriftQuarantine.enabled` | Enable read-only OpenNebula image/VM metadata drift detection and temporary volume quarantine for local RWO attach/detach loops. | `true` | No |
| `driver.metadataDriftQuarantine.failureThreshold` | Matching metadata-drift failures required before the volume quarantine is active. | `2` | No |
| `driver.metadataDriftQuarantine.ttlSeconds` | Active quarantine duration after the threshold is reached. | `1800` | No |
| `driver.hostArtifactQuarantine.enabled` | Enable read-only quarantine when local `fs_lvm_ssh` attach failures indicate a stale host-side LV such as `lv-one-<vm>-<disk>`. | `true` | No |
| `driver.hostArtifactQuarantine.failureThreshold` | Matching host-artifact failures required before the VM/disk slot quarantine is active. | `1` | No |
| `driver.hostArtifactQuarantine.ttlSeconds` | Active host-artifact quarantine duration; after external repair, operators can also clear the matching `opennebula-csi-host-artifact-state` key to retry immediately. | `3600` | No |
| `driver.localDeviceRecovery.enabled` | Enable controller-driven same-node detach/reattach recovery after node-side local device discovery repeatedly fails. | `true` | No |
| `driver.localDeviceRecovery.minAttempts` | Missing-device reports required from a node before recovery is eligible. | `3` | No |
| `driver.localDeviceRecovery.minAgeSeconds` | Minimum age of the first missing-device report before recovery is eligible. | `60` | No |
| `driver.localDeviceRecovery.intervalSeconds` | Controller leader scan interval for `opennebula-csi-node-device-state`. | `15` | No |
| `driver.localDeviceRecovery.cooldownSeconds` | Cooldown between failed recovery attempts for the same volume/node report. | `300` | No |
| `driver.localDeviceRecovery.maxAttemptsPerVolume` | Maximum same-node recovery attempts recorded per report before operator intervention is required. | `2` | No |
| `driver.localRWOStaleMountRecovery.activePodRecovery` | Allow experimental recovery of stale local RWO mounts while a pod target is still active. Requires `featureGates.localRWOStaleMountRecovery=true`. | `false` | No |
| `driver.localRWOStaleMountRecovery.maxAttempts` | Maximum recovery attempts persisted per local RWO volume session. | `3` | No |
| `driver.localRWOStaleMountRecovery.backoffSeconds` | Backoff after a failed local RWO stale-mount recovery attempt. | `10` | No |
| `driver.lastNodePreference.enabled` | Enable soft last-node preference injection for eligible local single-writer pods. | `true` | No |
| `driver.lastNodePreference.policy` | Last-node preference policy. `local-single-writer` is the supported `v0.4.7` policy. | `"local-single-writer"` | No |
| `driver.lastNodePreference.webhook.enabled` | Enable the mutating admission webhook service and configuration. | `true` | No |
| `driver.lastNodePreference.webhook.port` | HTTPS port exposed by the controller pod for webhook traffic. | `9443` | No |
| `driver.lastNodePreference.webhook.failurePolicy` | Admission webhook failure policy. | `"Ignore"` | No |
| `driver.stuckAttachmentReconciler.enabled` | Enable conservative stale attachment reconciliation in the controller leader. | `true` | No |
| `driver.stuckAttachmentReconciler.intervalSeconds` | Reconciler scan interval. | `60` | No |
| `driver.stuckAttachmentReconciler.orphanGraceSeconds` | Grace before detaching orphan or divergent OpenNebula attachments. | `120` | No |
| `driver.stuckAttachmentReconciler.staleVolumeAttachmentGraceSeconds` | Grace before deleting a stale attached `VolumeAttachment`. | `90` | No |
| `driver.adaptiveTimeout.enabled` | Enable adaptive timeout recommendations from recent hotplug observations. | `true` | No |
| `driver.adaptiveTimeout.minSamples` | Minimum successful samples before adaptive tuning activates. | `8` | No |
| `driver.adaptiveTimeout.sampleWindow` | Rolling observation window size per operation/backend/size bucket. | `20` | No |
| `driver.adaptiveTimeout.p95MultiplierPercent` | Multiplier applied to observed p95 latency to form the recommendation. | `400` | No |
| `driver.adaptiveTimeout.maxSeconds` | Maximum adaptive timeout recommendation. | `1800` | No |
| `driver.allowedDatastoreTypes` | Allowed backend types for provisioning. | `["local","ceph","cephfs"]` | No |
| `driver.extraArgs` | Extra CLI args appended to both controller and node driver containers. | `[]` | No |
| `driver.env` | Additional environment variables appended to both controller and node driver containers. Useful for advanced overrides such as `ONE_CSI_NODE_TOPOLOGY_SYSTEM_DS`. | `[]` | No |

At least one datastore source must be configured through `driver.defaultDatastores` or StorageClass `parameters.datastoreIDs`.

### Feature Gates

| Parameter | Description | Default | Required |
| --- | --- | --- | --- |
| `featureGates.compatibilityAwareSelection` | Enable compatibility-aware filtering for datastores such as `COMPATIBLE_SYS_DS`. | `true` | No |
| `featureGates.detachedDiskExpansion` | Enable detached persistent-disk expansion through image-level resize. Stable and enabled by default in `v0.4.3`. | `true` | No |
| `featureGates.cephfsExpansion` | Enable CephFS dynamic subvolume expansion. Stable and enabled by default in `v0.4.3`. | `true` | No |
| `featureGates.cephfsSnapshots` | Enable CephFS snapshot RPC flows. | `false` | No |
| `featureGates.cephfsClones` | Enable CephFS PVC clone and snapshot restore flows. | `false` | No |
| `featureGates.cephfsSelfHealing` | Enable stale CephFS mount lazy-unmount/remount recovery in node stage. `NodeGetVolumeStats` still reports disconnected CephFS mounts as restage-needed errors because kubelet stats calls do not include remount credentials. | `false` | No |
| `featureGates.cephfsPersistentRecovery` | Persist node-local CephFS session state, scan for stale mounts after node-plugin restart, and enqueue async recovery when volume stats detect a stale CephFS mount. | `true` | No |
| `featureGates.cephfsKernelMounts` | Allow CephFS StorageClasses to request `cephfsMounter=kernel`. Host kernel CephFS client support is still required. | `false` | No |
| `featureGates.localRWOStaleMountRecovery` | Enable local RWO stale mount detection and recovery from persisted node-side disk sessions. Active-pod recovery also requires `driver.localRWOStaleMountRecovery.activePodRecovery=true`. | `false` | No |
| `featureGates.topologyAccessibility` | Enable topology capability advertisement and `accessible_topology` handling. | `false` | No |

### Controller

| Parameter | Description | Default | Required |
| --- | --- | --- | --- |
| `controller.replicaCount` | Number of controller replicas. | `1` | No |
| `controller.sidecarTimeoutSeconds` | CSI sidecar RPC timeout used for the provisioner, attacher, and resizer. Set this above the maximum hotplug timeout budget. | `960` | No |
| `controller.podAnnotations` | Extra annotations for the controller pod template. | `{}` | No |
| `controller.resources` | Controller pod resource requests and limits. | `{}` | No |
| `controller.nodeSelector` | Node selector for the controller StatefulSet. | `{}` | No |
| `controller.tolerations` | Tolerations for the controller StatefulSet. | `[]` | No |
| `controller.affinity` | Affinity rules for the controller StatefulSet. | `{}` | No |
| `controller.extraArgs` | Extra CLI args appended only to the controller driver container. | `[]` | No |
| `controller.extraEnv` | Extra environment variables appended only to the controller driver container. | `[]` | No |

### Node

| Parameter | Description | Default | Required |
| --- | --- | --- | --- |
| `node.podAnnotations` | Extra annotations for the node DaemonSet pod template. | `{}` | No |
| `node.resources` | Node pod resource requests and limits. | `{}` | No |
| `node.nodeSelector` | Node selector for the node DaemonSet. | `{}` | No |
| `node.tolerations` | Tolerations for the node DaemonSet. | `[{"operator":"Exists"}]` | No |
| `node.affinity` | Affinity rules for the node DaemonSet. | `{}` | No |
| `node.extraArgs` | Extra CLI args appended only to the node driver container. | `[]` | No |
| `node.extraEnv` | Extra environment variables appended only to the node driver container. | `[]` | No |

### Metrics

| Parameter | Description | Default | Required |
| --- | --- | --- | --- |
| `metrics.enabled` | Enable sidecar metrics ports and metrics Services. | `true` | No |
| `metrics.path` | HTTP path used by ServiceMonitors for driver-native metrics. | `"/metrics"` | No |
| `metrics.driver.enabled` | Expose driver-native metrics server from controller and node pods. | `true` | No |
| `metrics.driver.port` | Driver-native metrics container and Service port. | `9810` | No |
| `metrics.controller.service.annotations` | Extra annotations for the controller metrics Service. | `{}` | No |
| `metrics.controller.service.labels` | Extra labels for the controller metrics Service. | `{}` | No |
| `metrics.controller.serviceMonitor.enabled` | Create a Prometheus Operator ServiceMonitor for controller metrics. | `false` | No |
| `metrics.controller.serviceMonitor.namespace` | Namespace for the controller ServiceMonitor. Empty means release namespace. | `""` | No |
| `metrics.controller.serviceMonitor.interval` | Scrape interval for the controller ServiceMonitor. | `"30s"` | No |
| `metrics.controller.serviceMonitor.scrapeTimeout` | Scrape timeout for the controller ServiceMonitor. | `"10s"` | No |
| `metrics.controller.serviceMonitor.labels` | Extra labels for the controller ServiceMonitor. | `{}` | No |
| `metrics.controller.serviceMonitor.annotations` | Extra annotations for the controller ServiceMonitor. | `{}` | No |
| `metrics.node.service.annotations` | Extra annotations for the node metrics Service. | `{}` | No |
| `metrics.node.service.labels` | Extra labels for the node metrics Service. | `{}` | No |
| `metrics.node.serviceMonitor.enabled` | Create a Prometheus Operator ServiceMonitor for node metrics. | `false` | No |
| `metrics.node.serviceMonitor.namespace` | Namespace for the node ServiceMonitor. Empty means release namespace. | `""` | No |
| `metrics.node.serviceMonitor.interval` | Scrape interval for the node ServiceMonitor. | `"30s"` | No |
| `metrics.node.serviceMonitor.scrapeTimeout` | Scrape timeout for the node ServiceMonitor. | `"10s"` | No |
| `metrics.node.serviceMonitor.labels` | Extra labels for the node ServiceMonitor. | `{}` | No |
| `metrics.node.serviceMonitor.annotations` | Extra annotations for the node ServiceMonitor. | `{}` | No |
| `metrics.ports.provisioner` | `csi-provisioner` metrics port. | `8685` | No |
| `metrics.ports.attacher` | `csi-attacher` metrics port. | `8686` | No |
| `metrics.ports.resizer` | `csi-resizer` metrics port. | `8687` | No |
| `metrics.ports.registrar` | `csi-node-driver-registrar` metrics port. | `8688` | No |

### Inventory Datastore Status

When `inventoryController.enabled=true`, `kubectl get opennebuladatastores` becomes the primary datastore inventory view.

- object names remain stable as `ds-<id>`
- the displayed `Name` column uses the OpenNebula datastore name
- `Status` values are:
  - `Enabled`: healthy and explicitly referenced by at least one StorageClass
  - `Available`: healthy, not explicitly disabled, and not explicitly referenced by any StorageClass
  - `Disabled`: explicitly disabled on the datastore object
  - `Unavailable`: unhealthy, missing, invalid, or backend-mismatched
- `Capacity` is rendered as `{available} / {total} ({usedPercent}%)`
- `Metrics` shows a compact fio summary when validation succeeded, otherwise `-`

Validation remains informational only and does not by itself turn a datastore unavailable.

### Inventory Operator Controls

- `OpenNebulaDatastore.spec.maintenanceMode=true` blocks new provisioning for that datastore while leaving attach, detach, and expand for existing volumes alone
- `OpenNebulaDatastore.status.storageClassDetails` surfaces StorageClass binding and expansion risks, including local `Immediate` warnings
- `OpenNebulaNode.status.displayState`, `systemDatastoreDisplay`, and hotplug fields make incident triage easier from `kubectl get opennebulanodes`

### Inventory Validation Profiles

The chart now ships reusable manual validation profiles under `inventoryController.validation.profiles`:

- `smoke`
- `throughput`
- `latency`

These are defaults for manual runs. They do not enable scheduled validation.

To trigger a one-shot datastore benchmark run directly, create an `OpenNebulaDatastoreBenchmarkRun`. If you use `metadata.name: auto`, the inventory controller replaces it with a concrete object named `ds-<id>-<timestamp>` and starts the fio Job for you.

Benchmark defaults:

- CephFS datastores default PVC access mode to `ReadWriteMany`
- all other datastores default PVC access mode to `ReadWriteOnce`
- benchmark Jobs default `activeDeadlineSeconds` to `900`
- terminal benchmark runs clean up their Job and PVC resources
- `spec.fioArgs` is additive: required fio defaults stay in place unless you override them explicitly

```bash
kubectl apply -f - <<EOF
apiVersion: storageprovider.opennebula.sparkaiur.io/v1alpha1
kind: OpenNebulaDatastoreBenchmarkRun
metadata:
  name: auto
spec:
  datastoreID: 1
  storageClassName: opennebula-default-rwo
  size: 1Gi
  accessModes:
    - ReadWriteOnce
  activeDeadlineSeconds: 900
  fioArgs:
    - --name=smoke
    - --rw=randrw
    - --bs=4k
    - --iodepth=8
    - --runtime=30
    - --time_based=1
EOF
```

Check the run and the resulting datastore metrics with:

```bash
kubectl get opennebuladatastorebenchmarkruns -o wide
kubectl get opennebuladatastores -o wide
```

### Operator Commands

The driver image includes cluster-operator modes that work well with a kubeconfig or in-cluster execution:

- `--mode=inventory-validate`
  - triggers a manual datastore validation run and waits for the result
  - accepts `--access-modes=ReadWriteOnce` or `--access-modes=ReadWriteMany` when the benchmark PVC mode must be explicit
- `--mode=support-bundle`
  - emits a JSON support bundle with inventory, hotplug, local-device recovery, StorageClass, volume-health, VolumeAttachment, and event data
- `--mode=volume-health`
  - emits focused JSON diagnostics for a volume selected by `--volume-id`, `--pv`, or `--pvc namespace/name`

### Preflight

| Parameter | Description | Default | Required |
| --- | --- | --- | --- |
| `preflight.enabled` | Create the optional preflight Job. | `false` | No |
| `preflight.failReleaseOnError` | Add Helm hook annotations so failed preflight blocks the release. | `true` | No |
| `preflight.image.repository` | Override image repository for the preflight Job. Empty falls back to `image.repository`. | `""` | No |
| `preflight.image.tag` | Override image tag for the preflight Job. Empty falls back to `image.tag`. | `""` | No |
| `preflight.datastores` | Datastore identifiers checked by preflight. | `[]` | No |
| `preflight.localImmediateBindingPolicy` | Policy for local-backed StorageClasses using `Immediate` binding during preflight. Supported values: `warn`, `fail`. | `"warn"` | No |
| `preflight.nodeStageSecretRefs` | `namespace/name` secret refs validated for CephFS node stage. | `[]` | Conditional |
| `preflight.provisionerSecretRefs` | `namespace/name` secret refs validated for CephFS provisioning. | `[]` | Conditional |

`preflight.nodeStageSecretRefs` and `preflight.provisionerSecretRefs` are only needed when validating CephFS secret wiring.
For local-backed classes, preflight now checks `volumeBindingMode` and warns by default if `Immediate` is used instead of `WaitForFirstConsumer`.

### Snapshotter

| Parameter | Description | Default | Required |
| --- | --- | --- | --- |
| `snapshotter.enabled` | Deploy the `csi-snapshotter` sidecar in the controller pod. | `true` | No |
| `snapshotter.image` | Snapshotter image reference. | `"registry.k8s.io/sig-storage/csi-snapshotter:v8.2.1"` | No |
| `snapshotter.extraArgs` | Additional args for the snapshotter sidecar. | `[]` | No |

### StorageClasses

`storageClasses` is an array. Each item renders one `StorageClass`.

| Parameter | Description | Default | Required |
| --- | --- | --- | --- |
| `storageClasses[].name` | StorageClass name. | none | Yes |
| `storageClasses[].annotations` | Extra annotations on the StorageClass. | none | No |
| `storageClasses[].labels` | Extra labels on the StorageClass. | none | No |
| `storageClasses[].reclaimPolicy` | StorageClass reclaim policy. | none | Conditional |
| `storageClasses[].allowVolumeExpansion` | Whether PVC resize is allowed for that StorageClass. | none | No |
| `storageClasses[].mountOptions` | StorageClass mount options. | none | No |
| `storageClasses[].volumeBindingMode` | StorageClass binding mode. | none | No |
| `storageClasses[].parameters` | Driver parameters injected into the StorageClass. | none | No |

Common `storageClasses[].parameters` used by this driver:

- `datastoreIDs`
- `datastoreSelectionPolicy`
- `fsType`
- `sharedFilesystemPath`
- `sharedFilesystemSubvolumeGroup`
- `cephfsMounter`
- `csi.storage.k8s.io/provisioner-secret-name`
- `csi.storage.k8s.io/provisioner-secret-namespace`
- `csi.storage.k8s.io/node-stage-secret-name`
- `csi.storage.k8s.io/node-stage-secret-namespace`
- OpenNebula disk tuning keys such as `cache`, `driver`, `io`, `devPrefix`

### Required Values by Scenario

| Scenario | Minimum required values |
| --- | --- |
| Basic disk-backed install | `oneApiEndpoint` plus either `credentials.existingSecret.name` or `credentials.inlineAuth` |
| Default provisioning without StorageClass overrides | `driver.defaultDatastores` |
| StorageClass-managed provisioning | `storageClasses[].name` plus `storageClasses[].parameters.datastoreIDs` or `driver.defaultDatastores` |
| CephFS filesystem provisioning | CephFS datastore IDs, StorageClass secret refs, Kubernetes Secrets with `adminID/adminKey` and `userID/userKey` |
| Topology accessibility alpha | `featureGates.topologyAccessibility=true` plus node labels `topology.opennebula.sparkaiur.io/system-ds=<id>` |
| Detached disk expansion | Enabled by default |
| CephFS expansion | Enabled by default |
| CephFS snapshots alpha | `featureGates.cephfsSnapshots=true` |
| CephFS clones alpha | `featureGates.cephfsClones=true` |

## Useful Commands

Render locally:

```bash
helm template opennebula-csi ./helm/opennebula-csi \
  --set credentials.existingSecret.name=opennebula-csi-auth
```

Lint locally:

```bash
helm lint ./helm/opennebula-csi \
  --set credentials.existingSecret.name=opennebula-csi-auth
```

Upgrade with a values file:

```bash
helm upgrade --install opennebula-csi ./helm/opennebula-csi \
  --namespace kube-system \
  --create-namespace \
  --set credentials.existingSecret.name=opennebula-csi-auth \
  --values examples/omni-values.yaml
```

## Related Examples

- `examples/helm-values-existing-secret.yaml`
- `examples/helm-values-single-datastore.yaml`
- `examples/helm-values-multi-datastore.yaml`
- `examples/helm-values-cephfs-dynamic.yaml`
- `examples/helm-values-feature-gates-alpha.yaml`
