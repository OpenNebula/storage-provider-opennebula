[//]: # ( vim: set wrap : )

# OpenNebula Kubernetes CSI Driver

This repository provides an implementation of a [Container Storage Interface](https://kubernetes-csi.github.io/docs/) (CSI) driver for Kubernetes on OpenNebula environments, enabling the definition of persistent volumes in [CAPONE](https://github.com/OpenNebula/cluster-api-provider-opennebula) clusters.

Current implementation includes support for PersistentVolumes with `ReadWriteOnce` and `ReadOnlyMany` access modes, as well as `Filesystem` and `Block` volume modes.

## Documentation

* [Wiki Pages](https://github.com/OpenNebula/storage-provider-opennebula/wiki)

## Contributing

* [Development and issue tracking](https://github.com/OpenNebula/storage-provider-opennebula/issues)

## Contact Information

* [OpenNebula web site](https://opennebula.io)
* [Enterprise Services](https://opennebula.io/enterprise)

## License

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the License. You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the specific language governing permissions and limitations under the License.

## Author Information

Copyright 2002-2025, OpenNebula Project, OpenNebula Systems

## Acknowledgments