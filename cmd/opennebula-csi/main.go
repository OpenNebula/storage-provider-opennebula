/*
Copyright 2025, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/config"
	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/driver"
	inventorycontroller "github.com/SparkAIUR/storage-provider-opennebula/pkg/inventory/controller"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/mount-utils"
	"k8s.io/utils/exec"
	ctrl "sigs.k8s.io/controller-runtime"
)

var (
	driverName                   = flag.String("drivername", driver.DefaultDriverName, "CSI driver name")
	pluginEndpoint               = flag.String("endpoint", driver.DefaultGRPCServerEndpoint, "CSI plugin endpoint")
	nodeID                       = flag.String("nodeid", "", "Node ID")
	maxVolumesPerNode            = flag.Uint64("maxVolumesPerNode", 255, "Maximum number of volumes that can be attached to a node")
	mode                         = flag.String("mode", "driver", "Execution mode: driver, preflight, inventory-controller, inventory-validate, support-bundle, volume-health, or hotplug-diagnose")
	output                       = flag.String("output", "text", "Output format for preflight mode: text or json")
	preflightDatastores          = flag.String("preflight-datastores", "", "Comma-separated datastore identifiers to validate during preflight")
	preflightNodeStageSecrets    = flag.String("preflight-node-stage-secrets", "", "Comma-separated namespace/name secret references for CephFS node-stage validation")
	preflightProvisionerSecrets  = flag.String("preflight-provisioner-secrets", "", "Comma-separated namespace/name secret references for CephFS provisioner validation")
	requireSnapshotCRDs          = flag.Bool("require-snapshot-crds", false, "Require snapshot.storage.k8s.io CRDs during preflight")
	requireServiceMonitorCRDs    = flag.Bool("require-servicemonitor-crds", false, "Require monitoring.coreos.com ServiceMonitor CRD during preflight")
	inventoryValidateDatastore   = flag.Int("datastore-id", 0, "OpenNebula datastore ID for inventory-validate mode")
	inventoryValidateSC          = flag.String("storage-class", "", "StorageClass to use for inventory-validate mode")
	inventoryValidateSize        = flag.String("size", "", "Validation PVC size for inventory-validate mode")
	inventoryValidateAccessModes = flag.String("access-modes", "", "Comma-separated PVC access modes for inventory-validate mode")
	inventoryValidateFioArgs     = flag.String("fio-args", "", "Comma-separated fio args for inventory-validate mode")
	volumeHealthVolumeID         = flag.String("volume-id", "", "CSI volume ID for volume-health mode")
	volumeHealthPV               = flag.String("pv", "", "PersistentVolume name for volume-health mode")
	volumeHealthPVC              = flag.String("pvc", "", "namespace/name PersistentVolumeClaim for volume-health mode")
	hotplugDiagnoseNode          = flag.String("hotplug-node", "", "Optional Kubernetes node name for hotplug-diagnose mode")
)

func main() {
	klog.InitFlags(nil)
	_ = flag.Set("logtostderr", "true")
	ctrl.SetLogger(klog.Background())
	flag.Parse()

	config := config.LoadConfiguration()

	exitCode := handle(config)
	os.Exit(exitCode)
}

func handle(cfg config.CSIPluginConfig) int {
	mounter := mount.NewSafeFormatAndMount(
		mount.New(""), // using default linux mounter implementation
		exec.New(),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cancelChan := make(chan os.Signal, 1)
	signal.Notify(cancelChan, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-cancelChan
		cancel()
	}()

	switch *mode {
	case "driver":
		driverOptions := &driver.DriverOptions{
			NodeID:             *nodeID,
			DriverName:         *driverName,
			GRPCServerEndpoint: *pluginEndpoint,
			PluginConfig:       cfg,
			Mounter:            mounter,
		}
		driver := driver.NewDriver(driverOptions)
		if err := driver.Run(ctx); err != nil {
			klog.Errorf("Failed to run driver: %v", err)
			return 1
		}
		return 0
	case "preflight":
		nodeStageRefs, err := driver.ParseSecretRefsCSV(*preflightNodeStageSecrets)
		if err != nil {
			klog.Errorf("Invalid node stage secret references: %v", err)
			return 1
		}
		provisionerRefs, err := driver.ParseSecretRefsCSV(*preflightProvisionerSecrets)
		if err != nil {
			klog.Errorf("Invalid provisioner secret references: %v", err)
			return 1
		}
		opts := driver.PreflightOptions{
			Datastores:               splitCSV(*preflightDatastores),
			NodeStageSecretRefs:      nodeStageRefs,
			ProvisionerSecretRefs:    provisionerRefs,
			RequireSnapshotCRDs:      *requireSnapshotCRDs,
			RequireServiceMonitorCRD: *requireServiceMonitorCRDs,
		}
		if err := driver.RunPreflightCommand(ctx, cfg, exec.New(), opts, *output, os.Stdout); err != nil {
			klog.Errorf("Preflight failed: %v", err)
			return 1
		}
		return 0
	case "inventory-controller":
		namespace, _ := cfg.GetString(config.InventoryControllerNamespaceVar)
		validationEnabled, _ := cfg.GetBool(config.InventoryValidationEnabledVar)
		defaultImage, _ := cfg.GetString(config.InventoryValidationDefaultImageVar)
		if err := inventorycontroller.Run(ctx, cfg, inventorycontroller.Options{
			Namespace:         namespace,
			ValidationEnabled: validationEnabled,
			DefaultImage:      defaultImage,
		}); err != nil {
			klog.Errorf("Inventory controller failed: %v", err)
			return 1
		}
		return 0
	case "inventory-validate":
		if err := driver.RunInventoryValidateCommand(ctx, cfg, driver.InventoryValidateOptions{
			DatastoreID:  *inventoryValidateDatastore,
			StorageClass: *inventoryValidateSC,
			Size:         *inventoryValidateSize,
			AccessModes:  parsePersistentVolumeAccessModes(splitCSV(*inventoryValidateAccessModes)),
			FioArgs:      splitCSV(*inventoryValidateFioArgs),
		}, os.Stdout); err != nil {
			klog.Errorf("Inventory validation failed: %v", err)
			return 1
		}
		return 0
	case "support-bundle":
		if err := driver.RunSupportBundleCommand(ctx, cfg, os.Stdout); err != nil {
			klog.Errorf("Support bundle failed: %v", err)
			return 1
		}
		return 0
	case "volume-health":
		if err := driver.RunVolumeHealthCommand(ctx, cfg, driver.VolumeHealthOptions{
			VolumeID: *volumeHealthVolumeID,
			PVName:   *volumeHealthPV,
			PVC:      *volumeHealthPVC,
		}, os.Stdout); err != nil {
			klog.Errorf("Volume health inspection failed: %v", err)
			return 1
		}
		return 0
	case "hotplug-diagnose":
		if err := driver.RunHotplugDiagnoseCommand(ctx, cfg, driver.HotplugDiagnoseOptions{
			Node: *hotplugDiagnoseNode,
		}, os.Stdout); err != nil {
			klog.Errorf("Hotplug diagnosis failed: %v", err)
			return 1
		}
		return 0
	default:
		klog.Errorf("Unsupported mode %q", *mode)
		return 1
	}
}

func splitCSV(value string) []string {
	if value == "" {
		return nil
	}

	parts := strings.Split(value, ",")
	normalized := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		normalized = append(normalized, trimmed)
	}

	return normalized
}

func parsePersistentVolumeAccessModes(values []string) []corev1.PersistentVolumeAccessMode {
	if len(values) == 0 {
		return nil
	}
	modes := make([]corev1.PersistentVolumeAccessMode, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		modes = append(modes, corev1.PersistentVolumeAccessMode(trimmed))
	}
	return modes
}
