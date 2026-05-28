# SparkAI OpenNebula CSI

`storage-provider-opennebula` is the SparkAI fork of the OpenNebula CSI driver for Kubernetes. It publishes container images to `ghcr.io/sparkaiur/opennebula-csi:<tag>` and `docker.io/nudevco/opennebula-csi:<tag>`, and Helm charts to `https://sparkaiur.github.io/storage-provider-opennebula/charts/`.

This fork is focused on Omni deployments on OpenNebula and removes the old requirement that a datastore literally named `default` must exist for PVC provisioning.

## Current scope

- Provision PVCs on explicitly configured OpenNebula datastores.
- Support global driver datastore defaults and per-StorageClass overrides.
- Support `least-used`, `ordered`, and `autopilot` datastore selection policies.
- Support `Filesystem` and `Block` volume modes.
- Support `ReadWriteOnce`, `ReadOnlyMany`, and CephFS-backed filesystem volumes for `ReadWriteOnce`, `ReadOnlyMany`, and `ReadWriteMany`.
- Support CSI volume expansion for attached volumes, detached persistent disks, and dynamic CephFS subvolumes.
- Support `NodeGetVolumeStats`, driver-native Prometheus metrics, Kubernetes Events, and PV placement annotations.
- Support preflight validation through the binary and an optional Helm Job.
- Support CSI snapshots plus PVC-to-PVC clone for disk-backed OpenNebula volumes.
- Support gated topology accessibility plus CephFS snapshot/clone and self-healing flows.
- Support `local`, OpenNebula Ceph RBD, and SparkAI CephFS datastores.
- Keep the internal selection/provider structure ready for future `nfs` support.

## Validated Staging Matrix

The staging lab has validated the following matrix across recent releases:

- local RWO volumes on an OpenNebula image datastore
- Ceph RBD RWO volumes on a Ceph-backed image datastore with a Ceph system datastore
- CephFS static and dynamic RWX volumes on a SparkAI CephFS `FILE` datastore
- ROX clones on the disk-backed path

Staging-validated stable features:

- Local RWO create, mount, resize, detach, reattach, and delete cleanup
- Ceph RBD RWO create, mount, resize, detach, reattach, and delete cleanup
- CephFS static RWX create, publish across nodes, and retain-on-delete behavior
- CephFS dynamic RWX create, publish across nodes, resize, snapshot create/delete, clone, snapshot restore, and delete cleanup
- Disk-path CSI snapshots and PVC clones
- `NodeGetVolumeStats`, driver metrics, PV placement annotations, and Kubernetes Events
- `detachedDiskExpansion`
- `cephfsExpansion`

Features that remain gated by default:

- `cephfsSnapshots`
- `cephfsClones`
- `cephfsSelfHealing`
- `cephfsKernelMounts`
- `topologyAccessibility`

## Release artifacts

- Container images:
  `ghcr.io/sparkaiur/opennebula-csi:<tag>`
  `docker.io/nudevco/opennebula-csi:<tag>`
- Latest release: `v0.5.15`
- Helm repo: `https://sparkaiur.github.io/storage-provider-opennebula/charts/`
- Chart name: `opennebula-csi`
- Source repo: `https://github.com/SparkAIUR/storage-provider-opennebula`

## Datastore selection

The driver now resolves its provisioning datastores from two layers:

1. StorageClass parameters
2. Driver defaults from environment or Helm values

If both are present, the StorageClass wins.

### Supported control parameters

These parameters are reserved by the CSI driver:

- `datastoreIDs`: CSV list of datastore identifiers, for example `100`, `100,101,102`, or `default`
- `datastoreSelectionPolicy`: `least-used`, `ordered`, or `autopilot`
- `fsType`: filesystem hint for filesystem volumes

All other StorageClass parameters are passed through to the existing disk/image handling logic. That allows tuning values such as `devPrefix`, `cache`, `driver`, and similar OpenNebula disk options.

### Driver environment variables

- `ONE_CSI_DEFAULT_DATASTORES`: global CSV datastore list, for example `100,101`
- `ONE_CSI_DATASTORE_SELECTION_POLICY`: `least-used`, `ordered`, or `autopilot`
- `ONE_CSI_ALLOWED_DATASTORE_TYPES`: CSV list, default `local,ceph,cephfs`
- `ONE_CSI_FEATURE_GATES`: CSV feature-gate map for alpha features
- `ONE_CSI_METRICS_ENDPOINT`: driver-native metrics endpoint, default `:9810`
- `ONE_CSI_VM_HOTPLUG_TIMEOUT_SECONDS`: legacy alias for the base VM hotplug timeout, default `120`
- `ONE_CSI_VM_HOTPLUG_TIMEOUT_BASE_SECONDS`: base VM hotplug timeout, default `120`
- `ONE_CSI_VM_HOTPLUG_TIMEOUT_PER_100GI_SECONDS`: additional timeout per 100 GiB bucket, default `60`
- `ONE_CSI_VM_HOTPLUG_TIMEOUT_MAX_SECONDS`: maximum VM hotplug timeout, default `900`
- `ONE_CSI_VM_HOTPLUG_STUCK_VM_COOLDOWN_SECONDS`: cooldown after a VM stays stuck in hotplug through the full timeout, default `300`
- `ONE_CSI_LOCAL_RESTART_OPTIMIZATION_ENABLED`: enable best-effort same-node restart reuse for opted-in local PVCs, default `true`
- `ONE_CSI_LOCAL_RESTART_DETACH_GRACE_SECONDS`: default delayed-detach grace for opted-in local PVCs, default `90`
- `ONE_CSI_LOCAL_RESTART_DETACH_GRACE_MAX_SECONDS`: maximum delayed-detach grace allowed by per-PVC override, default `300`
- `ONE_CSI_LOCAL_RESTART_REQUIRE_NODE_READY`: require Kubernetes node and OpenNebula VM readiness before using delayed detach, default `true`

### Policy behavior

- `least-used`: sort eligible datastores by free capacity descending, then try them in that order
- `ordered`: use the configured order as-is
- `autopilot`: score eligible datastores by free-space ratio, absolute free space, recent provisioning failures, and current in-flight selections

If a candidate datastore does not have enough free capacity, the driver falls through to the next eligible datastore. If no configured datastore can satisfy the request, provisioning fails with a capacity error.
When the driver falls back to a later datastore, it emits `DatastoreFallback` events and records the chosen placement on the resulting PV annotations.

## Datastore category support

Provisioning targets must be OpenNebula datastore categories that can accept image allocation.

Current rules:

- `IMAGE`: supported for PVC provisioning
- `FILE`: supported for SparkAI CephFS filesystem datastores when the datastore template exposes the required CephFS attributes
- `SYSTEM`: rejected for `CreateVolume`

For OpenNebula local-style datastores, the driver treats `local`, `fs`, `fs_lvm`, and `fs_lvm_ssh` as local-compatible backends.

### Local datastore placement guidance

OpenNebula local-style image datastores are best treated as placement-sensitive, not portable.

Recommended operator pattern for local-backed StorageClasses:

- use `volumeBindingMode: WaitForFirstConsumer`
- keep `compatibilityAwareSelection=true`
- enable `topologyAccessibility` only if you can maintain correct node labels
- use local-backed classes for node-sticky RWO workloads, not for workloads that must move freely between nodes

Important limitation:

- the driver does not perform CSI-side host-to-host data migration for local-backed PVCs
- when a pod lands on a different node, Kubernetes waits for detach/attach, but the underlying storage must already be attachable by OpenNebula from the configured image datastore path
- if OpenNebula cannot resolve the source image path during attach, the fix is in datastore layout or transfer-manager configuration, not in CSI-side file sync
- the controller serializes attach/detach/expand operations per volume, allows only one active VM hotplug per node at a time, and returns retryable `Aborted` for later same-node requests instead of letting them time out in-line
- hotplug attach and detach use a size-aware timeout budget derived from the actual OpenNebula disk size
- node-side device discovery now uses a smaller dedicated timeout budget so healthy fast-path mounts fail and retry quickly instead of waiting through the full hotplug recovery budget
- if a VM stays non-ready through the full hotplug timeout, the controller places that VM into a temporary recovery cooldown and rejects new hotplug work with retryable `Unavailable`
- recreating local-backed StatefulSet workloads should still be treated as node-sticky; if the workload must move freely across nodes after recreation, use Ceph RBD or CephFS instead

### Restart optimization for local StatefulSets

The driver now supports an opt-in delayed-detach path for local-backed `ReadWriteOnce` PVCs. This is aimed at StatefulSet restarts where the replacement pod often lands back on the same node.

Behavior:

- `ControllerUnpublishVolume` does not detach immediately for opted-in local PVCs
- the disk stays attached for a short grace period, default `90s`
- if the replacement pod is published back to the same node during that window, the controller reuses the existing attachment and skips detach/reattach
- if the replacement pod lands on a different node, the controller cancels the grace and detaches immediately so normal attach can continue
- if Kubernetes asks to detach while an active pod or non-deleting `VolumeAttachment` still desires the same node, the controller treats that stale same-node detach as successful and leaves the disk attached
- if stale sticky state points at an old node where OpenNebula already reports the disk absent, the controller clears the sticky state instead of issuing another detach

This is best-effort same-node reuse only. The CSI driver does not force Kubernetes to reschedule the pod onto the previous node.

Opt in from a StatefulSet `volumeClaimTemplates` entry:

```yaml
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

Recommended workload-side guidance:

- use a local `RWO` StorageClass with `volumeBindingMode: WaitForFirstConsumer`
- prefer ordinary StatefulSet rollout/restart flows over delete-and-recreate workflows
- keep replica placement predictable with anti-affinity or topology spread when appropriate
- do not use this as a substitute for portable storage; use Ceph RBD or CephFS if the workload must move freely across nodes

### Additional CSI fast-path behavior

The local restart path includes a broader CSI performance and stability layer:

- the node plugin now prefers stable `/dev/disk/by-id` resolution using an explicit OpenNebula disk serial and keeps a short-lived in-memory device cache
- same-node hotplug work is queued fairly instead of failing fast with `node_busy`
- the controller can inject a soft last-node scheduling preference for local single-writer pods so StatefulSet restarts are more likely to land back on the previous node
- a conservative stuck-attachment reconciler repairs orphaned OpenNebula attachments and stale `VolumeAttachment` objects
- OpenNebulaNode `HotplugStuck` inventory status immediately pauses new hotplug work for that node until readiness gates and inventory diagnosis clear
- hotplug queue state snapshots are debounced during churn while empty-queue snapshots still flush immediately for operator visibility
- attach, detach, and device-resolution latencies now feed an adaptive timeout window so slower environments raise budgets without reducing the current static timeout floor

### Controller maintenance mode

For Talos/Kubernetes rolling maintenance on clusters with local RWO volumes, operators can pause cross-node PVC movement through the controller ConfigMap:

```bash
kubectl -n kube-system patch configmap opennebula-csi-hotplug-state \
  --type merge \
  -p '{"data":{"maintainenceMode":"true"}}'
```

The controller accepts both `maintainenceMode` and `maintenanceMode`. While active, it prepares local non-CephFS `ReadWriteOnce` volumes best-effort, publishes `maintainenceReady=true`, injects required same-node affinity for eligible pods, holds maintenance detaches without physical OpenNebula detach, and rejects cross-node attach attempts with retryable `Unavailable`.

When maintenance is complete, set the mode key back to `false`. The controller removes `maintainenceReady`/`maintenanceReady` and releases maintenance holds gradually using `driver.maintenanceMode.releaseMinSeconds` and `driver.maintenanceMode.releaseMaxSeconds`.

### OpenNebula metadata drift guard

For local non-CephFS `ReadWriteOnce` volumes, the controller now checks OpenNebula image and VM disk metadata before attempting a cross-node attach. If OpenNebula still reports the image owned by another VM, or a detach fails while OpenNebula still reports an owner, the driver classifies the condition as metadata drift and returns `FailedPrecondition` with the image ID, owner VM IDs, disk IDs, and target devices it could observe.

This guard is intentionally read-only. It does not edit the OpenNebula database or run host-level `virsh` repair. After repeated matching failures, the controller records a temporary volume quarantine in `opennebula-csi-volume-quarantine-state` so retry loops stop issuing new physical hotplug work while operators repair OpenNebula state.

Default controls:

- `ONE_CSI_METADATA_DRIFT_QUARANTINE_ENABLED=true`
- `ONE_CSI_METADATA_DRIFT_QUARANTINE_FAILURE_THRESHOLD=2`
- `ONE_CSI_METADATA_DRIFT_QUARANTINE_TTL_SECONDS=1800`

### Host artifact quarantine

For local `fs_lvm_ssh` attach failures, the controller now preserves stale host-artifact details from OpenNebula instead of returning only `failed to attach volume`. If the failure references a host-local LV such as `lv-one-161-2`, or a local attach failure can be tied to the predicted VM/disk slot, the driver classifies it as `host_local_lv_conflict`, records a read-only quarantine in `opennebula-csi-host-artifact-state`, and returns `FailedPrecondition` with the VM ID, disk ID, target, LV name, and repair hint.

This path is intentionally read-only. It blocks repeated hotplug attempts against the same VM/disk slot until an operator repairs the host artifact externally and clears the matching `opennebula-csi-host-artifact-state` key, or until the quarantine TTL expires.

Default controls:

- `ONE_CSI_HOST_ARTIFACT_QUARANTINE_ENABLED=true`
- `ONE_CSI_HOST_ARTIFACT_QUARANTINE_FAILURE_THRESHOLD=1`
- `ONE_CSI_HOST_ARTIFACT_QUARANTINE_TTL_SECONDS=3600`

New workload controls:

- pod or PVC opt-out annotation:
  `storage-provider.opennebula.sparkaiur.io/last-node-preference: "disabled"`

New support signals:

- support bundles now include sticky attachment state, volume quarantine state, host artifact quarantine state, hotplug queue state, OpenNebula metadata drift details, and adaptive timeout observations/recommendations
- controller/node metrics expose queue depth, device-resolution methods, last-node preference outcomes, reconciler actions, and adaptive timeout recommendations

Canonical annotation namespace:

- driver-owned PV/PVC annotations use `storage-provider.opennebula.sparkaiur.io/*`
- `storage-provider.opennebula.sparkaiur.io/last-attached-node` is the canonical last-node annotation consumed by the driver
- legacy `csi.opennebula.io/last-attached-node` is ignored and reported in `volume-health`/support-bundle annotation audits

## Access mode support

Current access mode matrix:

- `ReadWriteOnce`: supported on the disk path and on the CephFS shared-filesystem path for `Filesystem` volumes
- `ReadOnlyMany`: supported on the disk path and on the CephFS shared-filesystem path for `Filesystem` volumes
- `ReadWriteMany`: supported for `Filesystem` volumes on the CephFS shared-filesystem path
- `MULTI_NODE_SINGLE_WRITER`: supported for `Filesystem` volumes on the CephFS shared-filesystem path
- `Block` + multi-node write: not supported

Routing is determined by the requested volume capability plus the selected datastore backend:

- `Filesystem` volumes route to the CephFS shared-filesystem path when the StorageClass resolves exclusively to CephFS datastores or explicitly uses `sharedFilesystemPath` / `sharedFilesystemSubvolumeGroup`
- disk-backed and block-backed volumes stay on the OpenNebula disk/image path
- StorageClasses must not mix CephFS and disk/image datastore IDs

### Disk path vs shared-fs path

- Databases and other single-writer application data should still use the OpenNebula `IMAGE` datastore path
- Shared caches, model stores, and other multi-node RWX workloads can use the CephFS shared-filesystem path
- Single-node filesystem workloads can also use CephFS when portable filesystem semantics are preferred over attached-disk semantics
- `ReadOnlyMany` remains on the disk path
- CephFS expansion is enabled by default
- CephFS snapshots and clones remain implemented behind feature gates and stay alpha-off by default

## Ceph RBD support

Ceph support in this fork means OpenNebula Ceph **RBD** datastores. It does not include CephFS.

Current Ceph support matrix:

- Ceph Image Datastore + Ceph System Datastore
- Ceph Image Datastore + SSH System Datastore

The driver validates Ceph datastores before provisioning and also validates the target VM deployment mode before attach.

For Ceph Image Datastores the driver requires:

- `DS_MAD=ceph`
- `TM_MAD=ceph`
- `DISK_TYPE=RBD`
- `POOL_NAME`
- `CEPH_HOST`
- `CEPH_USER`
- `CEPH_SECRET`
- `BRIDGE_LIST`

For Ceph System Datastores in Ceph mode the driver requires:

- `TM_MAD=ceph`
- `DISK_TYPE=RBD`
- the same Ceph connection identity as the image datastore:
  `POOL_NAME`, `CEPH_HOST`, `CEPH_USER`, `CEPH_SECRET`
- if defined on both sides, `CEPH_CONF` and `CEPH_KEY` must also match

If the target VM is running on an SSH System Datastore, attach is allowed and the driver logs that OpenNebula SSH mode caveats apply.

### Ceph prerequisites

Operators must prepare the OpenNebula environment before using Ceph-backed StorageClasses:

- Ceph client tools must be installed on the OpenNebula frontend and hypervisor nodes.
- OpenNebula nodes using Ceph must be configured as Ceph clients.
- The libvirt secret referenced by `CEPH_SECRET` must exist on the hypervisor nodes.
- The required Ceph keyring/key material must be present where OpenNebula expects it.
- Ceph Image and System Datastores must be created in OpenNebula with the required attributes above.
- OpenNebula should inherit datastore attributes needed for VM disk deployment, typically:
  `CEPH_HOST`, `CEPH_SECRET`, `CEPH_USER`, `CEPH_CONF`, `POOL_NAME`, `DISK_TYPE`
- If you rely on formatted datablocks, the filesystem requested through `fsType` must be supported by the OpenNebula datastore drivers and `mkfs.<fs>` must be available on the relevant nodes.

Optional Ceph datastore attributes supported by the driver and documentation:

- `CEPH_CONF`
- `CEPH_KEY`
- `RBD_FORMAT`
- `EC_POOL_NAME`
- `CEPH_TRASH`

## CephFS Filesystem Support

CephFS support in this fork is a SparkAI shared-filesystem path. It is separate from the existing OpenNebula image-backed disk path.

CephFS routing requirements:

- volume capability must be `Filesystem`
- the selected datastore set must resolve exclusively as `cephfs`, or the StorageClass must explicitly use `sharedFilesystemPath` / `sharedFilesystemSubvolumeGroup`
- StorageClasses must not mix CephFS and disk/image datastore IDs

Required OpenNebula datastore template attributes for CephFS:

- `SPARKAI_CSI_SHARE_BACKEND=cephfs`
- `SPARKAI_CSI_CEPHFS_FS_NAME`
- `SPARKAI_CSI_CEPHFS_ROOT_PATH`
- `SPARKAI_CSI_CEPHFS_SUBVOLUME_GROUP`
- `CEPH_HOST`

Optional datastore template attributes:

- `CEPH_CONF`
- `SPARKAI_CSI_CEPHFS_MOUNT_OPTIONS`

CephFS StorageClass parameters:

- `sharedFilesystemPath`
  - if set, the claim is treated as a static existing CephFS path
  - if unset, the driver dynamically provisions a CephFS subvolume
- `sharedFilesystemSubvolumeGroup`
  - optional override for the datastore default
- `cephfsMounter`
  - optional, defaults to `fuse`
  - accepts `fuse` or `kernel`
  - `kernel` requires `cephfsKernelMounts=true` plus host kernel CephFS client support

CephFS secret references:

- dynamic provisioning and deletion use the standard CSI provisioner secret reference
- CephFS expansion uses the standard CSI controller-expand secret reference
- node-side mounts use the standard CSI node-stage secret reference

Expected CephFS secret keys:

- provisioner secrets: `adminID`, `adminKey`
- node-stage secrets: `userID`, `userKey`

### CephFS prerequisites

- the selected datastore must be an OpenNebula `FILE` datastore
- the datastore template must expose the SparkAI CephFS attributes above
- Ceph monitor endpoints in `CEPH_HOST` must be reachable from the controller and node plugin pods
- the runtime image now includes `ceph`, `ceph-fuse`, and related Ceph packages
- CephFS node-stage auth is provided through Kubernetes Secret refs, not host-global Ceph config
- static CephFS paths configured through `sharedFilesystemPath` must already exist; the driver does not create them
- dynamic CephFS provisioning requires a Ceph user with permission to create, getpath, and remove subvolumes in the configured filesystem and subvolume group
- dynamic CephFS expansion also requires `csi.storage.k8s.io/controller-expand-secret-name` and `csi.storage.k8s.io/controller-expand-secret-namespace` on the StorageClass, typically pointing at the same secret used for the provisioner path
- CephFS expansion, snapshot, and clone flows require the corresponding feature gates to be enabled before use
- `cephfsPersistentRecovery=true` is enabled by default and persists node-local CephFS session state for startup and periodic recovery scans
- the default CephFS node mount path remains `ceph-fuse`; `cephfsMounter=kernel` is opt-in
- on Omni/Talos, `cephfsMounter=kernel` requires CephFS kernel client support in the node image or system extensions on every worker that may mount the volume

### CephFS alpha feature gates

- `cephfsSnapshots=true` enables `CreateSnapshot`, `DeleteSnapshot`, and snapshot listing for dynamic CephFS subvolumes
- `cephfsClones=true` enables PVC clone from CephFS volumes and restore from CephFS snapshots
- `cephfsSelfHealing=true` enables one lazy-unmount/remount recovery attempt when node stage detects a stale CephFS mount during staging
- `cephfsPersistentRecovery=true` enables persistent node-local CephFS session recovery after node-plugin restarts and async repair on stale volume stats
- `cephfsKernelMounts=true` allows StorageClasses to request `cephfsMounter=kernel`
- `NodeGetVolumeStats` reports stale or disconnected CephFS mounts as `FailedPrecondition` so kubelet surfaces a precise restage-needed error instead of a generic internal stat failure
- static CephFS paths created with `sharedFilesystemPath` still reject expansion

## Volume expansion

The chart supports `allowVolumeExpansion`, and the controller deployment now includes the CSI external resizer sidecar required by Kubernetes for PVC resize workflows.

Current behavior:

- Controller expansion is supported for OpenNebula volumes that are attached to a VM.
- Filesystem expansion on the node for mounted filesystem volumes enforces a post-condition: `NodeExpandVolume` returns success only after the mounted filesystem reaches requested size (within a configured tolerance).
- If device or filesystem growth does not converge before timeout, `NodeExpandVolume` returns retriable `DeadlineExceeded` with requested/device/filesystem byte context.
- Block volumes do not require node-side filesystem expansion.
- CephFS shared-filesystem volumes support expansion for dynamic subvolumes by default.
- Static CephFS paths created with `sharedFilesystemPath` are not expandable.
- Detached-volume expansion is enabled by default and uses image-level resize via `one.image.update`.
- Shrinking remains unsupported for disk and CephFS paths.

Node resize convergence can be tuned with:

- `driver.nodeExpand.verifyTimeoutSeconds` (`ONE_CSI_NODE_EXPAND_VERIFY_TIMEOUT_SECONDS`, default `120`)
- `driver.nodeExpand.retryIntervalSeconds` (`ONE_CSI_NODE_EXPAND_RETRY_INTERVAL_SECONDS`, default `2`)
- `driver.nodeExpand.sizeToleranceBytes` (`ONE_CSI_NODE_EXPAND_SIZE_TOLERANCE_BYTES`, default `134217728`)

## Snapshots and clones

Stable disk-path data-management features:

- CSI `CreateSnapshot`, `DeleteSnapshot`, and `ListSnapshots` for OpenNebula image-backed volumes
- PVC-to-PVC clone through CSI `VolumeContentSource.volume`
- the chart can deploy the external `csi-snapshotter` sidecar

Current limitations:

- restoring a new PVC from an OpenNebula image snapshot is not supported by the current backend path
- CephFS snapshots and clones remain alpha-off by default, but can be enabled with `cephfsSnapshots=true` and `cephfsClones=true`
- CephFS clone restore requires a dynamic CephFS volume source or a CephFS snapshot source

## Topology accessibility

Topology accessibility remains alpha and is disabled by default.

When `topologyAccessibility=true`:

- the plugin advertises CSI `VOLUME_ACCESSIBILITY_CONSTRAINTS`
- the node plugin reads the Kubernetes Node label `topology.opennebula.sparkaiur.io/system-ds`
- `NodeGetInfo` reports that label as the node’s accessible topology segment
- `CreateVolume` returns `accessible_topology` when the selected datastore exposes deterministic `COMPATIBLE_SYS_DS` values

Operational requirements:

- label Kubernetes nodes with `topology.opennebula.sparkaiur.io/system-ds=<opennebula-system-datastore-id>`
- ensure the node DaemonSet service account can `get` `nodes`
- if you need an override during testing, set `ONE_CSI_NODE_TOPOLOGY_SYSTEM_DS` through `driver.env`

Example objects:

- VolumeSnapshotClass: [examples/volumesnapshotclass.yaml](examples/volumesnapshotclass.yaml)
- VolumeSnapshot: [examples/volumesnapshot.yaml](examples/volumesnapshot.yaml)
- PVC clone: [examples/pvc-clone.yaml](examples/pvc-clone.yaml)

## Metrics and Prometheus

The Helm chart can now expose Prometheus-scrapable CSI sidecar metrics and driver-native metrics.

Sidecar metrics are exposed for:

- `csi-provisioner`
- `csi-attacher`
- `csi-resizer`
- `csi-node-driver-registrar`

These metrics cover CSI controller and node operations such as provisioning, attaching, resizing, request latency, and sidecar work-queue behavior.

Driver-native metrics add low-cardinality counters and gauges for:

- operation totals and latency
- datastore selection outcomes
- attach validation outcomes
- CephFS subvolume operations
- snapshot operations
- preflight checks
- datastore free and total bytes

Important distinction:

- PVC object metrics should come from `kube-state-metrics`
- CSI operation metrics come from this chart

Examples of PVC object metrics you should expect from `kube-state-metrics`:

- `kube_persistentvolumeclaim_info`
- `kube_persistentvolumeclaim_status_phase`
- `kube_persistentvolumeclaim_resource_requests_storage_bytes`

Chart metrics behavior:

- metrics endpoints are enabled by default
- the chart creates scrape `Service` objects for controller and node metrics
- optional `ServiceMonitor` objects can be enabled for Prometheus Operator environments
- standard Prometheus annotation-based scraping can also be used through `metrics.controller.service.annotations` and `metrics.node.service.annotations`

Relevant Helm values:

```yaml
metrics:
  enabled: true
  path: /metrics
  driver:
    enabled: true
    port: 9810
  controller:
    service:
      annotations: {}
      labels: {}
    serviceMonitor:
      enabled: false
      namespace: ""
      interval: 30s
      scrapeTimeout: 10s
      labels: {}
      annotations: {}
  node:
    service:
      annotations: {}
      labels: {}
    serviceMonitor:
      enabled: false
      namespace: ""
      interval: 30s
      scrapeTimeout: 10s
      labels: {}
      annotations: {}
```

If you are using Prometheus Operator, set:

```yaml
metrics:
  controller:
    serviceMonitor:
      enabled: true
  node:
    serviceMonitor:
      enabled: true
```

Example values enabling driver metrics and ServiceMonitors:

- [examples/driver-metrics-servicemonitor-values.yaml](examples/driver-metrics-servicemonitor-values.yaml)

## Preflight validation

The binary now supports a preflight mode that validates OpenNebula connectivity, datastore configuration, Ceph tooling, monitor reachability, referenced Kubernetes secrets, and optional CRD dependencies.
For local-backed StorageClasses, preflight also warns by default when `volumeBindingMode: Immediate` is used instead of `WaitForFirstConsumer`. Set `ONE_CSI_PREFLIGHT_LOCAL_IMMEDIATE_BINDING_POLICY=fail` to make that a hard release-blocking error.

CLI example:

```bash
./opennebula-csi \
  --mode=preflight \
  --output=text \
  --preflight-datastores=111,300 \
  --preflight-node-stage-secrets=kube-system/cephfs-node-stage \
  --preflight-provisioner-secrets=kube-system/cephfs-provisioner \
  --require-snapshot-crds \
  --require-servicemonitor-crds
```

Helm can also run preflight as an optional Job, including as a release-blocking hook:

- [examples/helm-values-preflight.yaml](examples/helm-values-preflight.yaml)

## Feature gates

Stable features are enabled by default. Higher-risk features remain behind feature gates.

Current gates:

- `compatibilityAwareSelection=true`
- `detachedDiskExpansion=true`
- `cephfsExpansion=true`
- `cephfsSnapshots=false`
- `cephfsClones=false`
- `cephfsSelfHealing=false`
- `cephfsPersistentRecovery=true`
- `cephfsKernelMounts=false`
- `topologyAccessibility=false`

The chart renders these into `ONE_CSI_FEATURE_GATES`.

- `detachedDiskExpansion` enables image-level resize for detached persistent disks
- `cephfsExpansion` enables dynamic CephFS subvolume quota resize
- `cephfsSnapshots` enables CephFS snapshot RPCs
- `cephfsClones` enables CephFS PVC clone and snapshot restore flows
- `cephfsSelfHealing` controls stale CephFS remount attempts after a stale mount is detected during node stage
- `cephfsPersistentRecovery` controls node-local CephFS session persistence, startup recovery scans, and async stale-mount repair attempts
- `cephfsKernelMounts` allows StorageClasses to request `cephfsMounter=kernel`
- `topologyAccessibility` enables node topology lookup and `accessible_topology` responses

## Helm installation

Add the SparkAI chart repo:

```bash
helm repo add sparkai-opennebula https://sparkaiur.github.io/storage-provider-opennebula/charts/
helm repo update
```

### Option 1: Existing Secret

Create a Secret with the OpenNebula credentials:

```bash
kubectl -n kube-system create secret generic opennebula-csi-auth \
  --from-literal=credentials='oneadmin:changeme'
```

Install the chart:

```bash
helm upgrade --install opennebula-csi sparkai-opennebula/opennebula-csi \
  --namespace kube-system \
  --create-namespace \
  --values examples/helm-values-existing-secret.yaml
```

### Option 2: Inline credentials

This is supported for convenience, but an existing Secret is the recommended production path.

```bash
helm upgrade --install opennebula-csi sparkai-opennebula/opennebula-csi \
  --namespace kube-system \
  --create-namespace \
  --values examples/helm-values-single-datastore.yaml
```

## Helm values model

The chart no longer renders a fixed set of StorageClasses. Instead it accepts a `storageClasses[]` list and flexible deployment settings.

### Key values

- `credentials.existingSecret.name`
- `credentials.existingSecret.key`
- `credentials.inlineAuth`
- `driver.defaultDatastores`
- `driver.datastoreSelectionPolicy`
- `driver.allowedDatastoreTypes`
- `driver.env`
- `controller.resources`
- `controller.nodeSelector`
- `controller.tolerations`
- `controller.affinity`
- `controller.podAnnotations`
- `controller.extraArgs`
- `controller.extraEnv`
- `node.resources`
- `node.nodeSelector`
- `node.tolerations`
- `node.affinity`
- `node.podAnnotations`
- `node.extraArgs`
- `node.extraEnv`
- `imagePullSecrets`
- `storageClasses[]`
- `featureGates`
- `metrics.driver`
- `preflight`
- `snapshotter`

### StorageClass schema

Each item under `storageClasses` supports:

- `name`
- `annotations`
- `labels`
- `reclaimPolicy`
- `allowVolumeExpansion`
- `mountOptions`
- `volumeBindingMode`
- `parameters`

## Example values files

- Single datastore by ID: [examples/helm-values-single-datastore.yaml](examples/helm-values-single-datastore.yaml)
- Multiple datastores with `least-used`: [examples/helm-values-multi-datastore.yaml](examples/helm-values-multi-datastore.yaml)
- Existing Secret with `default` alias: [examples/helm-values-existing-secret.yaml](examples/helm-values-existing-secret.yaml)
- Omni-oriented example: [examples/omni-values.yaml](examples/omni-values.yaml)
- Ceph RBD with Ceph System Datastore mode: [examples/helm-values-ceph-ceph-mode.yaml](examples/helm-values-ceph-ceph-mode.yaml)
- Ceph RBD with SSH System Datastore mode: [examples/helm-values-ceph-ssh-mode.yaml](examples/helm-values-ceph-ssh-mode.yaml)
- CephFS static RWX example: [examples/helm-values-cephfs-static.yaml](examples/helm-values-cephfs-static.yaml)
- CephFS dynamic RWX example: [examples/helm-values-cephfs-dynamic.yaml](examples/helm-values-cephfs-dynamic.yaml)
- Alpha feature-gate example: [examples/helm-values-feature-gates-alpha.yaml](examples/helm-values-feature-gates-alpha.yaml)
- Snapshotter/clone example values: [examples/helm-values-snapshotter.yaml](examples/helm-values-snapshotter.yaml)
- Preflight job example values: [examples/helm-values-preflight.yaml](examples/helm-values-preflight.yaml)
- CephFS node-stage secret example: [examples/cephfs-node-stage-secret.yaml](examples/cephfs-node-stage-secret.yaml)
- CephFS provisioner secret example: [examples/cephfs-provisioner-secret.yaml](examples/cephfs-provisioner-secret.yaml)
- Driver metrics + ServiceMonitor example values: [examples/driver-metrics-servicemonitor-values.yaml](examples/driver-metrics-servicemonitor-values.yaml)
- VolumeSnapshotClass example: [examples/volumesnapshotclass.yaml](examples/volumesnapshotclass.yaml)
- VolumeSnapshot example: [examples/volumesnapshot.yaml](examples/volumesnapshot.yaml)
- PVC clone example: [examples/pvc-clone.yaml](examples/pvc-clone.yaml)
- CephFS RWX demo workload: [examples/demo-busybox-cephfs-rwx.yaml](examples/demo-busybox-cephfs-rwx.yaml)
- Omni-oriented Ceph example: [examples/omni-values-ceph.yaml](examples/omni-values-ceph.yaml)
- Staging-lab Omni Ceph example: [examples/omni-values-staging-ceph.yaml](examples/omni-values-staging-ceph.yaml)

## Omni deployment notes

For Omni on OpenNebula, the common pattern is:

1. Create or reference an existing Secret with `ONE_AUTH`
2. Set `driver.defaultDatastores` to the datastores that Omni should consume
3. Create one or more StorageClasses under `storageClasses[]`
4. Use StorageClass-specific `parameters` to tune the underlying datastore behavior

The driver currently validates configured provisioning datastores against the allowed datastore type list, which defaults to `local,ceph,cephfs`.
For OpenNebula local-style datastores, the driver treats `local`, `fs`, `fs_lvm`, and `fs_lvm_ssh` as local-compatible.
Provisioning targets still need to be OpenNebula `IMAGE` or `FILE` datastores. `SYSTEM` datastores cannot be used for `CreateVolume`.
If you want PVC resizing in Omni, set `allowVolumeExpansion: true` on the relevant StorageClasses and ensure workloads are using attached volumes when expansion is requested.
If you need CephFS-backed filesystem semantics, use a CephFS-enabled StorageClass with node-stage and provisioner secrets wired in.

For Ceph-backed Omni deployments:

1. Create the OpenNebula Ceph Image Datastore and, if needed, the Ceph System Datastore.
2. Ensure Ceph prerequisites are satisfied on the frontend and hypervisor nodes.
3. Set `driver.defaultDatastores` or StorageClass `datastoreIDs` to the Ceph Image Datastore IDs.
4. Keep `driver.allowedDatastoreTypes` including `ceph`.
5. If workloads run in SSH System Datastore mode, expect OpenNebula SSH mode limitations for Ceph-backed images.

For CephFS-backed Omni deployments:

1. Create a `FILE` datastore in OpenNebula and add the SparkAI CephFS attributes to its template.
2. Set `driver.defaultDatastores` or StorageClass `datastoreIDs` to the CephFS datastore IDs.
3. Add node-stage and provisioner secret refs to the StorageClass parameters.
4. Use `sharedFilesystemSubvolumeGroup` for dynamic CephFS provisioning or `sharedFilesystemPath` for static CephFS provisioning.
5. Keep the default `cephfsMounter=fuse` unless the Omni/Talos node image already exposes kernel CephFS client support. Only then enable `cephfsKernelMounts=true` and set `cephfsMounter=kernel`.
6. Use `ReadWriteOnce`, `ReadOnlyMany`, or `ReadWriteMany` on the PVC as needed.
7. Validate the resulting deployment with [examples/demo-busybox-cephfs-rwx.yaml](examples/demo-busybox-cephfs-rwx.yaml).

## Staging validation gate

The repo includes a staging validation gate used on a live Omni + Talos + OpenNebula cluster before release:

- a local validation script: `hack/validate-staging-cephfs.sh`
- a manual GitHub Actions workflow: `.github/workflows/staging-cephfs-validation.yaml`
- the workflow builds and pushes an ephemeral `ghcr.io/sparkaiur/opennebula-csi:staging-<sha>` image before deploying, so staging always exercises the branch under test instead of the last tagged release

Local staging runs can override the deployed image with:

- `CSI_IMAGE_REPOSITORY`
- `CSI_IMAGE_TAG`

Required staging secrets for the workflow:

- `STAGING_KUBECONFIG_B64`
- `STAGING_ONE_XMLRPC`
- `STAGING_ONE_AUTH`
- `STAGING_CEPHFS_ADMIN_ID`
- `STAGING_CEPHFS_ADMIN_KEY`
- `STAGING_CEPHFS_NODE_USER_ID`
- `STAGING_CEPHFS_NODE_USER_KEY`

Sanitized staging-derived operator notes:

- local-style datastores should use `WaitForFirstConsumer` and be treated as placement-sensitive, not portable
- Ceph RBD volumes require the target VM to run on a compatible Ceph system datastore; a local `TM_MAD=local` system datastore is not a valid Ceph RBD attach target
- static CephFS paths must already exist before the claim is created
- dynamic CephFS expansion requires the standard CSI controller-expand secret refs on the StorageClass
- stale CephFS mounts surface as restage-needed errors in volume stats; with `cephfsPersistentRecovery=true` the node plugin now also queues an async remount/rebind attempt so most pods recover after the next kubelet retry instead of waiting for manual restage

## Local development

Build the binary:

```bash
go build ./cmd/opennebula-csi
```

Run the Go test suite:

```bash
go test ./...
```

Render the chart with example values:

```bash
helm lint helm/opennebula-csi
helm template opennebula-csi helm/opennebula-csi --values examples/helm-values-multi-datastore.yaml
helm template opennebula-csi helm/opennebula-csi --values examples/helm-values-ceph-ceph-mode.yaml
```

### Opt-in test suites

These suites are disabled by default because they require real infrastructure:

- `RUN_CSI_SANITY_TESTS=1 go test ./pkg/csi/driver -run TestDriver`
- `RUN_OPENNEBULA_INTEGRATION_TESTS=1 go test ./pkg/csi/opennebula`
- `RUN_OPENNEBULA_E2E_TESTS=1 go test ./pkg/csi/test/e2e`

## Release flow

Every semantic release must be validated in the `hplcsi` lab before the tag is created.

Required release gate:

1. Build the candidate image from the branch under test
2. Deploy that candidate image into the lab cluster, not the previous tagged release
3. Validate the feature area touched by the change on live infrastructure
4. Only after lab validation passes, create or move the semantic tag and publish the release

At minimum, release validation should include:

- `go test ./...`
- `helm template opennebula-csi ./helm/opennebula-csi ...`
- `bash hack/validate-release-lab.sh`
- a live lab validation for the feature or hotfix being released, including local attach/mount, local expansion, inventory CRDs, and workload bootstrap/init smoke checks when touching fast-path mount behavior

Push the semantic tag for the release being cut, for example `v0.5.15`, only after that validation to trigger the release workflow.

The workflow will:

1. Build and publish `ghcr.io/sparkaiur/opennebula-csi:<tag>` and `latest`
2. Build and publish `docker.io/nudevco/opennebula-csi:<tag>` and `latest`
3. Package the Helm chart
4. Publish the chart tarball and `index.yaml` to `gh-pages/charts/`
5. Re-index the Helm repo with `https://sparkaiur.github.io/storage-provider-opennebula/charts/`
6. Create a GitHub release for the tag

## Inventory Datastore Status

When the inventory controller is enabled, `kubectl get opennebuladatastores` is the operator-facing datastore inventory view.

- object names remain stable as `ds-<id>`
- the displayed datastore `Name` comes from OpenNebula, not `metadata.name`
- `Status` values mean:
  - `Enabled`: healthy and explicitly referenced by at least one StorageClass
  - `Available`: healthy, not explicitly disabled, and not explicitly referenced by any StorageClass
  - `Disabled`: explicitly disabled through the datastore object
  - `Unavailable`: unhealthy, missing, invalid, or backend-mismatched
- `Capacity` is rendered as `{available} / {total} ({usedPercent}%)`
- `Metrics` shows a compact fio summary when validation succeeded, otherwise `-`

Validation remains informational only and does not by itself make a datastore unavailable.

## Inventory Operator Controls

- `spec.maintenanceMode=true` disables new provisioning for that datastore without renaming or deleting the object
- `spec.maintenanceMessage` is surfaced through datastore conditions so operators can see why provisioning is blocked
- `status.referencedByStorageClass`, `status.referenceCount`, and `status.storageClassDetails` make StorageClass risk visible directly in the CRD
- local-backed StorageClasses using `Immediate` are flagged in inventory and preflight
- `kubectl get opennebulanodes` now surfaces `displayState`, `systemDatastoreDisplay`, hotplug cooldown visibility, and attached-volume summaries

## Inventory Commands

The binary now includes operator-oriented modes in addition to the CSI and inventory-controller modes:

- `inventory-validate`
  - creates a datastore benchmark run, waits for completion, and prints a compact JSON summary
  - example:

```bash
go run ./cmd/opennebula-csi \
  --mode=inventory-validate \
  --datastore-id=111 \
  --storage-class=opennebula-default-rwo \
  --access-modes=ReadWriteOnce \
  --size=1Gi
```

- `support-bundle`
  - prints a JSON support snapshot including effective config, feature gates, leadership settings, hotplug cooldown snapshots, datastore/node inventory, StorageClass audit data, volume-health diagnostics, VolumeAttachments, and recent Events
  - example:

```bash
go run ./cmd/opennebula-csi --mode=support-bundle > support-bundle.json
```

- `volume-health`
  - prints focused JSON diagnostics for PV/PVC/VolumeAttachment plus any node-local disk or CephFS session state visible from the container
  - example:

```bash
go run ./cmd/opennebula-csi \
  --mode=volume-health \
  --pvc=s3/data1-minio-pool-0
```

## Upstream

This project is derived from the OpenNebula storage provider. The fork keeps the Apache 2.0 license and focuses on SparkAI-specific OpenNebula and Omni requirements.
