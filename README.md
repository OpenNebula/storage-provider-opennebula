[//]: # ( vim: set wrap : )

# OpenNebula Kubernetes CSI Driver

This repository provides an implementation of a [Container Storage Interface](https://kubernetes-csi.github.io/docs/) (CSI) driver for Kubernetes on OpenNebula environments, enabling persistent storage integration for Kubernetes clusters running on OpenNebula.

The driver supports dynamic provisioning of Kubernetes PersistentVolumes, Helm-based deployment, filesystem and block volume modes, and volume expansion. The validated storage paths include OpenNebula `IMAGE` datastore volumes for `ReadWriteOnce` workloads and CephFS `FILE` datastore volumes for `ReadWriteMany` workloads.

## Validated Storage Backends

| Backend | Access mode | Validation status | Notes |
|---|---:|---|---|
| Local `IMAGE` datastore | RWO | Validated | ext4/XFS expansion, detach/reattach, CSI requested-size metadata, stale `SIZE` recovery |
| Ceph RBD `IMAGE` datastore | RWO | Validated | ext4/XFS expansion and detach/reattach. OpenNebula canonical `SIZE` updated to the expanded size |
| CephFS `FILE` datastore | RWMany | Validated | Dynamic provisioning, shared mounts, expansion, pod churn, remount persistence, and cleanup |

For local `IMAGE` datastore RWO volumes, the driver persists CSI requested-size metadata and uses it to restore the effective expanded disk size during attach when the OpenNebula canonical image `SIZE` remains stale after expansion.

## Important Notes

* Local `IMAGE` datastore canonical OpenNebula image `SIZE` may remain stale after expansion. The driver uses CSI requested-size metadata to preserve the effective expanded size across detach and reattach.
* Detached local persistent disk expansion is rejected because it is unsafe. Expand local persistent disks while attached.
* CephFS RWMany controller-side expansion was validated. Kubernetes may report `NodeResizeError` because node-side expansion is not implemented for this shared filesystem path, while PVC/PV capacity updates and mounted CephFS clients still observe the expanded capacity.
* The Helm chart can deploy `csi-snapshotter`, but the sidecar is optional when `snapshotter.enabled=false`.

## Documentation

Detailed installation, StorageClass examples, backend-specific configuration, validation procedures, and troubleshooting notes are maintained in the project wiki:

* [Wiki home](https://github.com/OpenNebula/storage-provider-opennebula/wiki)
    * [Installation and Helm deployment](https://github.com/OpenNebula/storage-provider-opennebula/wiki/csi_install)
    * [Storage backends](https://github.com/OpenNebula/storage-provider-opennebula/wiki/csi_storage_backends)
    * [Local RWO expansion](https://github.com/OpenNebula/storage-provider-opennebula/wiki/csi_local_rwo_expansion)
    * [Ceph RBD RWO](https://github.com/OpenNebula/storage-provider-opennebula/wiki/csi_ceph_rbd_rwo)
    * [CephFS RWMany](https://github.com/OpenNebula/storage-provider-opennebula/wiki/csi_cephfs_rwmany)
    * [Validation matrix](https://github.com/OpenNebula/storage-provider-opennebula/wiki/csi_validation)
    * [Troubleshooting](https://github.com/OpenNebula/storage-provider-opennebula/wiki/csi_troubleshooting)

## Contributing

* [Development and issue tracking](https://github.com/OpenNebula/storage-provider-opennebula/issues)

## Contact Information

* [OpenNebula web site](https://opennebula.io)
* [Enterprise Services](https://opennebula.io/enterprise)

## License

Licensed under the Apache License, Version 2.0 (the "License"). You may not use this file except in compliance with the License. You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the specific language governing permissions and limitations under the License.

## Author Information

Copyright 2002-2025, OpenNebula Project, OpenNebula Systems

## Acknowledgments

This work includes functionality originally developed in the [SparkAIUR OpenNebula CSI fork](https://github.com/SparkAIUR/storage-provider-opennebula), which was imported, adapted, and validated for OpenNebula CSI driver use cases.
