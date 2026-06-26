package controller

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/OpenNebula/one/src/oca/go/src/goca"
	datastoreSchema "github.com/OpenNebula/one/src/oca/go/src/goca/schemas/datastore"
	"github.com/OpenNebula/one/src/oca/go/src/goca/schemas/shared"
	vmSchema "github.com/OpenNebula/one/src/oca/go/src/goca/schemas/vm"
	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/config"
	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/opennebula"
	inventoryv1alpha1 "github.com/SparkAIUR/storage-provider-opennebula/pkg/inventory/apis/storageprovider/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	crlog "sigs.k8s.io/controller-runtime/pkg/log"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

const (
	validationNamespace                                      = "kube-system"
	validationPVCPrefix                                      = "one-ds-validate-pvc-"
	validationJobPrefix                                      = "one-ds-validate-job-"
	benchmarkPVCPrefix                                       = "one-ds-bench-pvc-"
	benchmarkJobPrefix                                       = "one-ds-bench-job-"
	benchmarkFinalizer                                       = "storage-provider.opennebula.sparkaiur.io/benchmark-cleanup"
	benchmarkLabelComponent                                  = "app.kubernetes.io/component"
	benchmarkLabelRun                                        = "storage-provider.opennebula.sparkaiur.io/benchmark-run"
	benchmarkLabelDatastoreID                                = "storage-provider.opennebula.sparkaiur.io/datastore-id"
	benchmarkComponentValue                                  = "datastore-benchmark"
	defaultValidationActiveDeadlineSeconds int64             = 900
	eventReasonValidationTriggered                           = "ValidationTriggered"
	eventReasonValidationSucceeded                           = "ValidationSucceeded"
	eventReasonValidationFailed                              = "ValidationFailed"
	eventReasonProvisioningDisabled                          = "ProvisioningDisabled"
	eventReasonDiscoveryMismatch                             = "DiscoveryMismatch"
	eventReasonInventoryVMNotFound                           = "VMNotFound"
	eventReasonInventoryNodeNotFound                         = "NodeNotFound"
	annotationDatastoreID                                    = "storage-provider.opennebula.sparkaiur.io/datastore-id"
	topologySystemDSLabel                                    = "topology.opennebula.sparkaiur.io/system-ds"
	hotplugStateConfigMapName                                = "opennebula-csi-hotplug-state"
	hotplugDiagnosticsConfigMapName                          = "opennebula-csi-hotplug-diagnostics"
	sharedBackendAttr                                        = "SPARKAI_CSI_SHARE_BACKEND"
	defaultValidationImagePullPolicy       corev1.PullPolicy = corev1.PullIfNotPresent
)

type latestBenchmarkResult struct {
	RunName string
	Status  inventoryv1alpha1.OpenNebulaDatastoreBenchmarkRunStatus
}

type Options struct {
	Namespace         string
	ValidationEnabled bool
	DefaultImage      string
}

type Syncer struct {
	client                 ctrlclient.Client
	apiReader              ctrlclient.Reader
	scheme                 *runtime.Scheme
	kube                   kubernetes.Interface
	ctrl                   *goca.Controller
	namespace              string
	resyncDatastores       time.Duration
	resyncNodes            time.Duration
	validationEnabled      bool
	defaultValidationImage string
	hotplugDiagnosisConfig opennebula.HotplugDiagnosisConfig
}

func Run(ctx context.Context, cfg config.CSIPluginConfig, options Options) error {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := storagev1.AddToScheme(scheme); err != nil {
		return err
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		return err
	}
	if err := inventoryv1alpha1.AddToScheme(scheme); err != nil {
		return err
	}

	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("failed to initialize in-cluster config for inventory controller: %w", err)
	}

	metricsAddr := "0"
	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		LeaderElection:         true,
		LeaderElectionID:       loadInventoryLeaderElectionID(cfg),
		HealthProbeBindAddress: ":8081",
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
	})
	if err != nil {
		return err
	}

	endpoint, ok := cfg.GetString(config.OpenNebulaRPCEndpointVar)
	if !ok {
		return fmt.Errorf("failed to get %s endpoint from config", config.OpenNebulaRPCEndpointVar)
	}
	credentials, ok := cfg.GetString(config.OpenNebulaCredentialsVar)
	if !ok {
		return fmt.Errorf("failed to get %s credentials from config", config.OpenNebulaCredentialsVar)
	}

	kube, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return err
	}

	syncer := &Syncer{
		client:                 mgr.GetClient(),
		apiReader:              mgr.GetAPIReader(),
		scheme:                 scheme,
		kube:                   kube,
		ctrl:                   goca.NewController(opennebula.NewClient(opennebula.OpenNebulaConfig{Endpoint: endpoint, Credentials: credentials}).Client),
		namespace:              options.Namespace,
		resyncDatastores:       time.Duration(loadResync(cfg, config.InventoryControllerResyncDatastoresVar, 60)) * time.Second,
		resyncNodes:            time.Duration(loadResync(cfg, config.InventoryControllerResyncNodesVar, 30)) * time.Second,
		validationEnabled:      options.ValidationEnabled,
		defaultValidationImage: options.DefaultImage,
		hotplugDiagnosisConfig: loadInventoryHotplugDiagnosisConfig(cfg),
	}

	if err := mgr.Add(syncer); err != nil {
		return err
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}

	return mgr.Start(ctx)
}

func (s *Syncer) NeedLeaderElection() bool {
	return true
}

func (s *Syncer) Start(ctx context.Context) error {
	log := crlog.FromContext(ctx).WithName("inventory-sync")
	log.Info("starting inventory sync loops",
		"resyncDatastores", s.resyncDatastores.String(),
		"resyncNodes", s.resyncNodes.String(),
		"validationEnabled", s.validationEnabled,
	)
	dsTicker := time.NewTicker(s.resyncDatastores)
	defer dsTicker.Stop()
	nodeTicker := time.NewTicker(s.resyncNodes)
	defer nodeTicker.Stop()

	if err := s.syncDatastores(ctx); err != nil {
		ctrl.Log.Error(err, "initial datastore inventory sync failed")
	}
	if err := s.syncNodes(ctx); err != nil {
		ctrl.Log.Error(err, "initial node inventory sync failed")
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-dsTicker.C:
			if err := s.syncDatastores(ctx); err != nil {
				ctrl.Log.Error(err, "datastore inventory sync failed")
			}
		case <-nodeTicker.C:
			if err := s.syncNodes(ctx); err != nil {
				ctrl.Log.Error(err, "node inventory sync failed")
			}
		}
	}
}

func (s *Syncer) syncDatastores(ctx context.Context) error {
	log := crlog.FromContext(ctx).WithName("inventory-datastores")
	dsPool, err := s.ctrl.Datastores().InfoContext(ctx)
	if err != nil {
		return err
	}
	pvs, err := s.kube.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	pvcs, err := s.kube.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	scs, err := s.kube.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	vas, err := s.kube.StorageV1().VolumeAttachments().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}

	pvcByNSName := make(map[string]corev1.PersistentVolumeClaim, len(pvcs.Items))
	for _, pvc := range pvcs.Items {
		pvcByNSName[pvc.Namespace+"/"+pvc.Name] = pvc
	}

	discoveredIDs := make(map[int]datastoreSchema.Datastore, len(dsPool.Datastores))
	for _, ds := range dsPool.Datastores {
		discoveredIDs[ds.ID] = ds
		if err := s.ensureDatastoreObject(ctx, ds); err != nil {
			return err
		}
	}
	if err := s.reconcileBenchmarkRuns(ctx, discoveredIDs, scs.Items); err != nil {
		return err
	}
	latestBenchmarks, err := s.loadLatestBenchmarkResults(ctx)
	if err != nil {
		return err
	}

	var dsList inventoryv1alpha1.OpenNebulaDatastoreList
	if err := s.apiReader.List(ctx, &dsList); err != nil {
		return err
	}
	log.Info("reconciling datastore inventory", "discoveredCount", len(dsPool.Datastores), "objectCount", len(dsList.Items))

	for _, item := range dsList.Items {
		ds, ok := discoveredIDs[item.Spec.Discovery.OpenNebulaDatastoreID]
		base := item.DeepCopy()
		status := item.DeepCopy()
		if !ok {
			status.Status = inventoryv1alpha1.OpenNebulaDatastoreStatus{
				ObservedGeneration:    item.Generation,
				Phase:                 inventoryv1alpha1.DatastorePhaseUnavailable,
				Health:                inventoryv1alpha1.DatastoreHealthNotFound,
				ID:                    item.Spec.Discovery.OpenNebulaDatastoreID,
				Name:                  datastoreDisplayName(item, item.Status.OpenNebula),
				Backend:               item.Spec.Discovery.ExpectedBackend,
				CapacityDisplay:       unknownCapacityDisplay,
				StorageClassesDisplay: "-",
				MetricsDisplay:        validationMetricsDisplay(item.Status.Validation),
				ValidationLastOutcome: validationLastOutcome(item.Status.Validation),
				LastValidationAge:     validationAgeDisplay(item.Status.Validation),
				LastValidationPhase:   validationPhaseDisplay(item.Status.Validation),
				LastValidationSummary: validationSummaryDisplay(item.Status.Validation),
				Validation:            item.Status.Validation,
				Conditions: []metav1.Condition{
					newCondition("Discovered", metav1.ConditionFalse, "DatastoreMissing", "Datastore was not found in OpenNebula"),
				},
			}
			log.Info("patching datastore status",
				"datastoreObject", item.Name,
				"datastoreID", item.Spec.Discovery.OpenNebulaDatastoreID,
				"phase", status.Status.Phase,
				"health", status.Status.Health,
			)
			if err := s.client.Status().Patch(ctx, status, ctrlclient.MergeFrom(base)); err != nil {
				log.Error(err, "failed to patch datastore status", "datastoreObject", item.Name, "datastoreID", item.Spec.Discovery.OpenNebulaDatastoreID)
				return err
			}
			log.Info("patched datastore status",
				"datastoreObject", item.Name,
				"datastoreID", item.Spec.Discovery.OpenNebulaDatastoreID,
				"phase", status.Status.Phase,
				"name", status.Status.Name,
				"backend", status.Status.Backend,
				"capacityDisplay", status.Status.CapacityDisplay,
			)
			continue
		}
		nextStatus := s.buildDatastoreStatus(item, ds, pvs.Items, pvcByNSName, scs.Items, vas.Items, latestBenchmarks[ds.ID])
		status.Status = nextStatus
		log.Info("patching datastore status",
			"datastoreObject", item.Name,
			"datastoreID", nextStatus.ID,
			"phase", nextStatus.Phase,
			"health", nextStatus.Health,
		)
		if err := s.client.Status().Patch(ctx, status, ctrlclient.MergeFrom(base)); err != nil {
			log.Error(err, "failed to patch datastore status", "datastoreObject", item.Name, "datastoreID", nextStatus.ID)
			return err
		}
		log.Info("patched datastore status",
			"datastoreObject", item.Name,
			"datastoreID", nextStatus.ID,
			"phase", nextStatus.Phase,
			"name", nextStatus.Name,
			"backend", nextStatus.Backend,
			"capacityDisplay", nextStatus.CapacityDisplay,
		)
		if s.validationEnabled {
			if err := s.reconcileValidation(ctx, status); err != nil {
				ctrl.Log.Error(err, "failed reconciling datastore validation", "datastore", status.Name)
			}
		}
	}
	return nil
}

func (s *Syncer) syncNodes(ctx context.Context) error {
	log := crlog.FromContext(ctx).WithName("inventory-nodes")
	nodes, err := s.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	vas, err := s.kube.StorageV1().VolumeAttachments().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	pvs, err := s.kube.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}

	pvByName := make(map[string]corev1.PersistentVolume, len(pvs.Items))
	for _, pv := range pvs.Items {
		pvByName[pv.Name] = pv
	}

	for _, node := range nodes.Items {
		if err := s.ensureNodeObject(ctx, node); err != nil {
			return err
		}
	}

	var nodeList inventoryv1alpha1.OpenNebulaNodeList
	if err := s.client.List(ctx, &nodeList); err != nil {
		return err
	}
	log.Info("reconciling node inventory", "kubernetesNodeCount", len(nodes.Items), "objectCount", len(nodeList.Items))

	dsNames := make(map[int]string)
	if datastores, err := s.ctrl.Datastores().InfoContext(ctx); err != nil {
		log.Error(err, "failed to list OpenNebula datastores for node inventory")
	} else {
		dsNames = datastoreNameMap(datastores)
	}
	hotplugStates, _ := s.loadHotplugStateMap(ctx)
	hotplugObservations, _ := s.loadHotplugObservationMap(ctx)

	nodeByName := make(map[string]corev1.Node, len(nodes.Items))
	for _, node := range nodes.Items {
		nodeByName[node.Name] = node
	}

	for _, item := range nodeList.Items {
		status := item.DeepCopy()
		node, ok := nodeByName[item.Name]
		if !ok {
			status.Status = inventoryv1alpha1.OpenNebulaNodeStatus{
				ObservedGeneration: item.Generation,
				Phase:              inventoryv1alpha1.NodePhaseNotFound,
				DisplayState:       "NodeMissing",
				Conditions: []metav1.Condition{
					newCondition("KubernetesNodeFound", metav1.ConditionFalse, "NodeMissing", "Kubernetes node was not found"),
				},
			}
			if err := s.client.Status().Update(ctx, status); err != nil {
				return err
			}
			continue
		}
		nextStatus := s.buildNodeStatus(ctx, item, node, vas.Items, pvByName, dsNames, hotplugStates, hotplugObservations)
		status.Status = nextStatus
		if err := s.client.Status().Update(ctx, status); err != nil {
			return err
		}
	}

	return nil
}

func (s *Syncer) ensureDatastoreObject(ctx context.Context, ds datastoreSchema.Datastore) error {
	name := datastoreObjectName(ds.ID)
	current := &inventoryv1alpha1.OpenNebulaDatastore{}
	err := s.client.Get(ctx, types.NamespacedName{Name: name}, current)
	if err == nil {
		desiredBackend := normalizeDatastoreBackend(ds)
		if current.Spec.Discovery.ExpectedBackend != desiredBackend {
			updated := current.DeepCopy()
			updated.Spec.Discovery.ExpectedBackend = desiredBackend
			if err := s.client.Update(ctx, updated); err != nil {
				return err
			}
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	obj := &inventoryv1alpha1.OpenNebulaDatastore{
		TypeMeta: metav1.TypeMeta{
			APIVersion: inventoryv1alpha1.SchemeGroupVersion.String(),
			Kind:       "OpenNebulaDatastore",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: inventoryv1alpha1.OpenNebulaDatastoreSpec{
			DisplayName: ds.Name,
			Enabled:     true,
			Validation: inventoryv1alpha1.OpenNebulaDatastoreValidationSpec{
				Mode: inventoryv1alpha1.DatastoreValidationModeDisabled,
			},
			Discovery: inventoryv1alpha1.OpenNebulaDatastoreDiscoverySpec{
				OpenNebulaDatastoreID: ds.ID,
				ExpectedBackend:       normalizeDatastoreBackend(ds),
				Allowed:               true,
			},
		},
	}
	return s.client.Create(ctx, obj)
}

func (s *Syncer) ensureNodeObject(ctx context.Context, node corev1.Node) error {
	current := &inventoryv1alpha1.OpenNebulaNode{}
	err := s.client.Get(ctx, types.NamespacedName{Name: node.Name}, current)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	obj := &inventoryv1alpha1.OpenNebulaNode{
		TypeMeta: metav1.TypeMeta{
			APIVersion: inventoryv1alpha1.SchemeGroupVersion.String(),
			Kind:       "OpenNebulaNode",
		},
		ObjectMeta: metav1.ObjectMeta{Name: node.Name},
		Spec: inventoryv1alpha1.OpenNebulaNodeSpec{
			OpenNebulaVMName: node.Name,
			Enabled:          true,
		},
	}
	return s.client.Create(ctx, obj)
}

func (s *Syncer) buildDatastoreStatus(item inventoryv1alpha1.OpenNebulaDatastore, ds datastoreSchema.Datastore, pvs []corev1.PersistentVolume, pvcByNSName map[string]corev1.PersistentVolumeClaim, scs []storagev1.StorageClass, vas []storagev1.VolumeAttachment, benchmark *latestBenchmarkResult) inventoryv1alpha1.OpenNebulaDatastoreStatus {
	normalized := normalizeDatastore(ds)
	status := inventoryv1alpha1.OpenNebulaDatastoreStatus{
		ObservedGeneration: item.Generation,
		ID:                 normalized.ID,
		Name:               datastoreDisplayName(item, inventoryv1alpha1.OpenNebulaDatastoreOpenNebulaStatus{ID: normalized.ID, Name: normalized.Name, Type: normalized.Type}),
		Type:               normalized.Type,
		Backend:            normalized.Backend,
		OpenNebula: inventoryv1alpha1.OpenNebulaDatastoreOpenNebulaStatus{
			ID:                         normalized.ID,
			Name:                       normalized.Name,
			State:                      datastoreStateString(ds),
			DSMad:                      normalized.DSMad,
			TMMad:                      normalized.TMMad,
			Type:                       normalized.Type,
			CompatibleSystemDatastores: append([]int(nil), normalized.CompatibleSystemDatastores...),
		},
		Capacity: inventoryv1alpha1.OpenNebulaDatastoreCapacityStatus{
			TotalBytes:      normalized.TotalBytes,
			FreeBytes:       normalized.FreeBytes,
			UsedBytesApprox: maxInt64(normalized.TotalBytes-normalized.FreeBytes, 0),
		},
	}

	storageClasses := make(map[string]struct{})
	attachedHandles := make(map[string]struct{})
	for _, va := range vas {
		if va.Spec.Source.PersistentVolumeName != nil {
			attachedHandles[*va.Spec.Source.PersistentVolumeName] = struct{}{}
		}
	}

	for _, pv := range pvs {
		pvDatastoreID, ok := datastoreIDFromPV(pv)
		if !ok || pvDatastoreID != normalized.ID {
			continue
		}
		status.Usage.PersistentVolumeCount++
		if quantity, ok := pv.Spec.Capacity[corev1.ResourceStorage]; ok {
			status.Capacity.ProvisionedPVBytes += quantity.Value()
		}
		if _, ok := attachedHandles[pv.Name]; ok {
			status.Usage.AttachedVolumeCount++
		}
		if pv.Spec.StorageClassName != "" {
			storageClasses[pv.Spec.StorageClassName] = struct{}{}
		}
		if pv.Spec.ClaimRef != nil {
			status.Usage.BoundPVCCount++
			status.Usage.BoundClaims = append(status.Usage.BoundClaims, inventoryv1alpha1.ObjectRefSummary{
				Namespace: pv.Spec.ClaimRef.Namespace,
				Name:      pv.Spec.ClaimRef.Name,
				UID:       string(pv.Spec.ClaimRef.UID),
			})
			if pvc, ok := pvcByNSName[pv.Spec.ClaimRef.Namespace+"/"+pv.Spec.ClaimRef.Name]; ok {
				if quantity, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
					status.Capacity.RequestedPVCBytes += quantity.Value()
				}
			}
		}
	}
	referencedByStorageClass := false
	for _, sc := range scs {
		if storageClassUsesDatastore(sc, normalized.ID) {
			storageClasses[sc.Name] = struct{}{}
			referencedByStorageClass = true
		}
	}
	status.Usage.StorageClasses = sortedKeys(storageClasses)
	status.ReferencedByStorageClass = referencedByStorageClass
	status.ReferenceCount = int32(len(status.Usage.StorageClasses))
	status.Validation = item.Status.Validation
	status.StorageClassesDisplay = storageClassesDisplay(status.Usage.StorageClasses)
	status.CapacityDisplay = formatCapacityDisplay(status.Capacity.FreeBytes, status.Capacity.TotalBytes)
	status.MetricsDisplay = validationMetricsDisplay(status.Validation)
	if benchmark != nil {
		if summary := benchmarkSummaryDisplay(benchmark.Status); summary != "-" {
			status.MetricsDisplay = summary
		}
	}
	status.Health = datastoreHealthForDisplay(status.Capacity.TotalBytes, item.Spec.Discovery.ExpectedBackend, normalized.Backend)
	status.Phase = datastorePhaseForDisplay(status.Health, item.Spec.Enabled, item.Spec.Discovery.Allowed, item.Spec.MaintenanceMode, referencedByStorageClass)
	status.ValidationEligible = validationEligible(item, normalized.Backend)
	status.ValidationLastOutcome = validationLastOutcome(status.Validation)
	status.LastValidationAge = validationAgeDisplay(status.Validation)
	status.LastValidationPhase = validationPhaseDisplay(status.Validation)
	status.LastValidationSummary = validationSummaryDisplay(status.Validation)
	status.StorageClassDetails = buildStorageClassDetails(scs, normalized)
	status.Conditions = datastoreConditions(item, normalized, status, benchmark)
	return status
}

func (s *Syncer) buildNodeStatus(ctx context.Context, item inventoryv1alpha1.OpenNebulaNode, node corev1.Node, vas []storagev1.VolumeAttachment, pvByName map[string]corev1.PersistentVolume, datastoreNames map[int]string, hotplugStates map[string]opennebula.HotplugCooldownState, hotplugObservations map[string]opennebula.HotplugObservation) inventoryv1alpha1.OpenNebulaNodeStatus {
	log := crlog.FromContext(ctx).WithName("inventory-node-status")
	status := inventoryv1alpha1.OpenNebulaNodeStatus{
		ObservedGeneration: item.Generation,
		Kubernetes: inventoryv1alpha1.OpenNebulaNodeKubernetesStatus{
			Name:       node.Name,
			UID:        string(node.UID),
			ProviderID: node.Spec.ProviderID,
			Labels:     filteredNodeLabels(node.Labels),
			Taints:     append([]corev1.Taint(nil), node.Spec.Taints...),
			Addresses:  append([]corev1.NodeAddress(nil), node.Status.Addresses...),
			Conditions: convertNodeConditions(node.Status.Conditions),
		},
		Storage: inventoryv1alpha1.OpenNebulaNodeStorageStatus{
			TopologySystemDatastore: node.Labels[topologySystemDSLabel],
		},
	}

	vmName := item.Spec.OpenNebulaVMName
	if strings.TrimSpace(vmName) == "" {
		vmName = node.Name
	}
	vmID, resolutionPath, err := opennebula.ResolveNodeVMID(ctx, s.ctrl, vmName)
	if err != nil {
		log.Info("resolved node vm", "node", node.Name, "vmName", vmName, "resolutionPath", resolutionPath, "resolvedVMID", -1, "error", err.Error())
		status.Phase = inventoryv1alpha1.NodePhaseNotFound
		if _, ok := err.(*opennebula.VMDuplicateNameError); ok {
			status.DisplayState = "VMNotFound"
			status.Conditions = []metav1.Condition{
				newCondition("KubernetesNodeFound", metav1.ConditionTrue, "NodeFound", "Kubernetes node was found"),
				newCondition("OpenNebulaVMFound", metav1.ConditionFalse, "VMDuplicateMatch", "Multiple OpenNebula VMs matched the node name"),
			}
			return status
		}
		status.DisplayState = "VMNotFound"
		status.Conditions = []metav1.Condition{
			newCondition("KubernetesNodeFound", metav1.ConditionTrue, "NodeFound", "Kubernetes node was found"),
			newCondition("OpenNebulaVMFound", metav1.ConditionFalse, "VMNotFound", "OpenNebula VM was not found"),
		}
		return status
	}
	log.Info("resolved node vm", "node", node.Name, "vmName", vmName, "resolutionPath", resolutionPath, "resolvedVMID", vmID)
	vmInfo, err := s.ctrl.VM(vmID).InfoContext(ctx, true)
	if err != nil {
		status.Phase = inventoryv1alpha1.NodePhaseDegraded
		status.DisplayState = "VMNotReady"
		status.Conditions = []metav1.Condition{
			newCondition("KubernetesNodeFound", metav1.ConditionTrue, "NodeFound", "Kubernetes node was found"),
			newCondition("OpenNebulaVMFound", metav1.ConditionFalse, "VMLookupFailed", "OpenNebula VM lookup failed"),
		}
		return status
	}
	vmState, vmLCM, _ := vmInfo.State()
	systemDSID, _ := latestHistoryDatastoreIDForInventory(vmInfo)
	hostID := 0
	hostName := ""
	if len(vmInfo.HistoryRecords) > 0 {
		latest := vmInfo.HistoryRecords[len(vmInfo.HistoryRecords)-1]
		hostID = latest.HID
		hostName = latest.Hostname
	}
	status.OpenNebula = inventoryv1alpha1.OpenNebulaNodeOpenNebulaStatus{
		VMID:                vmInfo.ID,
		VMName:              vmInfo.Name,
		State:               vmState.String(),
		LCMState:            vmLCM.String(),
		HostID:              hostID,
		HostName:            hostName,
		SystemDatastoreID:   systemDSID,
		SystemDatastoreName: datastoreNames[systemDSID],
	}
	status.Hotplug.Ready = vmLCM == vmSchema.Running
	status.Hotplug.InCooldown = false
	diagnosis, observation := opennebula.ClassifyHotplugObservation(
		opennebula.BuildHotplugObservation(node.Name, vmInfo, nil),
		hotplugObservationPtr(hotplugObservations, node.Name),
		s.hotplugDiagnosisConfig,
	)
	status.Hotplug.Diagnosis = convertHotplugDiagnosisStatus(diagnosis)
	s.persistHotplugObservation(ctx, observation)
	if systemDSID != 0 || status.OpenNebula.SystemDatastoreName != "" {
		status.SystemDatastoreDisplay = fmt.Sprintf("%d:%s", systemDSID, status.OpenNebula.SystemDatastoreName)
	}
	if state, ok := hotplugStates[node.Name]; ok {
		status.Hotplug.InCooldown = state.PauseUntilReady || state.FailureCount == 0
		if !state.ExpiresAt.IsZero() {
			expires := metav1.NewTime(state.ExpiresAt)
			status.Hotplug.CooldownExpiresAt = &expires
		}
		status.Hotplug.LastCooldownOperation = state.Operation
		status.Hotplug.LastCooldownVolume = state.Volume
		attached := state.LastObservedAttached
		ready := state.LastObservedReady
		status.Hotplug.LastObservedAttached = &attached
		status.Hotplug.LastObservedReady = &ready
		status.Hotplug.FailureCount = state.FailureCount
		status.Hotplug.Reason = state.Reason
		status.Hotplug.PauseUntilReady = state.PauseUntilReady
		kubernetesReady := state.KubernetesReady
		openNebulaReady := state.OpenNebulaReady
		unschedulable := state.Unschedulable
		status.Hotplug.KubernetesReady = &kubernetesReady
		status.Hotplug.OpenNebulaReady = &openNebulaReady
		status.Hotplug.Unschedulable = &unschedulable
	}

	for _, va := range vas {
		if va.Spec.NodeName != node.Name || va.Spec.Source.PersistentVolumeName == nil {
			continue
		}
		status.ActiveHotplugPressure++
		pv, ok := pvByName[*va.Spec.Source.PersistentVolumeName]
		if !ok || pv.Spec.CSI == nil {
			continue
		}
		status.Storage.AttachedVolumeCount++
		status.Storage.AttachedVolumeIDs = append(status.Storage.AttachedVolumeIDs, pv.Spec.CSI.VolumeHandle)
		diskID := persistentDiskIDFromVM(vmInfo, pv.Spec.CSI.VolumeHandle)
		summary := inventoryv1alpha1.AttachedDiskSummary{
			VolumeHandle:     pv.Spec.CSI.VolumeHandle,
			PersistentDiskID: diskID,
		}
		if pv.Spec.ClaimRef != nil {
			summary.ClaimNamespace = pv.Spec.ClaimRef.Namespace
			summary.ClaimName = pv.Spec.ClaimRef.Name
		}
		status.Storage.AttachedPersistentDisks = append(status.Storage.AttachedPersistentDisks, summary)
	}
	sort.Strings(status.Storage.AttachedVolumeIDs)
	status.AttachedVolumeHandlesDisplay = attachedVolumeHandlesDisplay(status.Storage.AttachedVolumeIDs)

	phase := inventoryv1alpha1.NodePhaseReady
	if !item.Spec.Enabled {
		phase = inventoryv1alpha1.NodePhaseDisabled
	} else if !status.Hotplug.Ready {
		phase = inventoryv1alpha1.NodePhaseDegraded
	} else if status.Hotplug.Diagnosis.Classification == opennebula.HotplugClassificationStuck {
		phase = inventoryv1alpha1.NodePhaseDegraded
	}
	status.Phase = phase
	status.DisplayState = nodeDisplayState(item.Spec.Enabled, status.Hotplug.InCooldown, status.Hotplug.Ready)
	switch status.Hotplug.Diagnosis.Classification {
	case opennebula.HotplugClassificationStuck:
		status.DisplayState = "HotplugStuck"
	case opennebula.HotplugClassificationProgressing:
		status.DisplayState = "HotplugProgressing"
	}
	status.Conditions = []metav1.Condition{
		newCondition("KubernetesNodeFound", metav1.ConditionTrue, "NodeFound", "Kubernetes node was found"),
		newCondition("OpenNebulaVMFound", metav1.ConditionTrue, "VMFound", "OpenNebula VM was found"),
		newCondition("VMReady", boolCondition(status.Hotplug.Ready), "VMReadinessEvaluated", "OpenNebula VM readiness evaluated"),
		newCondition("TopologyResolved", boolCondition(status.Storage.TopologySystemDatastore != ""), "TopologyEvaluated", "Topology label evaluated"),
		newCondition("HotplugCooldown", boolCondition(status.Hotplug.InCooldown), boolReason(status.Hotplug.InCooldown, "CooldownActive", "CooldownClear"), "Controller hotplug cooldown state evaluated"),
		newCondition("HotplugProgressing", boolCondition(status.Hotplug.Diagnosis.Classification == opennebula.HotplugClassificationProgressing), boolReason(status.Hotplug.Diagnosis.Classification == opennebula.HotplugClassificationProgressing, "HotplugProgressing", "HotplugNotProgressing"), status.Hotplug.Diagnosis.Message),
		newCondition("HotplugStuck", boolCondition(status.Hotplug.Diagnosis.Classification == opennebula.HotplugClassificationStuck), boolReason(status.Hotplug.Diagnosis.Classification == opennebula.HotplugClassificationStuck, "HotplugStuck", "HotplugNotStuck"), status.Hotplug.Diagnosis.Message),
		newCondition("HotplugPressureAcceptable", boolCondition(status.ActiveHotplugPressure < 8), boolReason(status.ActiveHotplugPressure < 8, "HotplugPressureNormal", "HotplugPressureHigh"), fmt.Sprintf("Node has %d active CSI attachment(s)", status.ActiveHotplugPressure)),
	}
	return status
}

func (s *Syncer) reconcileBenchmarkRuns(ctx context.Context, discovered map[int]datastoreSchema.Datastore, scs []storagev1.StorageClass) error {
	var runList inventoryv1alpha1.OpenNebulaDatastoreBenchmarkRunList
	if err := s.apiReader.List(ctx, &runList); err != nil {
		return err
	}
	for _, item := range runList.Items {
		if strings.EqualFold(item.Name, "auto") {
			if err := s.expandAutoBenchmarkRun(ctx, &item, scs); err != nil {
				return err
			}
			continue
		}

		current := item.DeepCopy()
		if !item.DeletionTimestamp.IsZero() {
			_ = s.cleanupValidationResources(ctx, current.Status.JobName, current.Status.PVCName)
			if hasString(current.Finalizers, benchmarkFinalizer) {
				before := current.DeepCopy()
				current.Finalizers = removeString(current.Finalizers, benchmarkFinalizer)
				if err := s.client.Patch(ctx, current, ctrlclient.MergeFrom(before)); err != nil {
					return err
				}
			}
			continue
		}
		if !hasString(current.Finalizers, benchmarkFinalizer) {
			before := current.DeepCopy()
			current.Finalizers = append(current.Finalizers, benchmarkFinalizer)
			if err := s.client.Patch(ctx, current, ctrlclient.MergeFrom(before)); err != nil {
				return err
			}
		}
		ds, ok := discovered[item.Spec.DatastoreID]
		if !ok {
			current.Status = inventoryv1alpha1.OpenNebulaDatastoreBenchmarkRunStatus{
				ObservedGeneration: item.Generation,
				Phase:              inventoryv1alpha1.ValidationPhaseFailed,
				DatastoreID:        item.Spec.DatastoreID,
				Message:            fmt.Sprintf("OpenNebula datastore %d was not found", item.Spec.DatastoreID),
			}
			if err := s.client.Status().Patch(ctx, current, ctrlclient.MergeFrom(item.DeepCopy())); err != nil {
				return err
			}
			continue
		}

		resolved := s.resolveBenchmarkRun(item, ds, scs)
		jobName := benchmarkResourceName(benchmarkJobPrefix, resolved)
		pvcName := benchmarkResourceName(benchmarkPVCPrefix, resolved)

		if isTerminalValidationPhase(current.Status.Phase) {
			if current.Status.ObservedGeneration == resolved.Generation {
				_ = s.cleanupValidationResources(ctx, jobName, pvcName)
				continue
			}
			_ = s.cleanupValidationResources(ctx, current.Status.JobName, current.Status.PVCName)
		}

		pvc := &corev1.PersistentVolumeClaim{}
		err := s.client.Get(ctx, types.NamespacedName{Namespace: s.namespace, Name: pvcName}, pvc)
		if err != nil && apierrors.IsNotFound(err) {
			pvcObj, buildErr := s.buildBenchmarkPVC(&resolved, ds, pvcName)
			if buildErr != nil {
				now := metav1.Now()
				current.Status = inventoryv1alpha1.OpenNebulaDatastoreBenchmarkRunStatus{
					ObservedGeneration: resolved.Generation,
					Phase:              inventoryv1alpha1.ValidationPhaseFailed,
					DatastoreID:        resolved.Spec.DatastoreID,
					DatastoreName:      ds.Name,
					Backend:            normalizeDatastoreBackend(ds),
					JobName:            jobName,
					PVCName:            pvcName,
					CompletedAt:        &now,
					Message:            buildErr.Error(),
				}
				if err := s.client.Status().Patch(ctx, current, ctrlclient.MergeFrom(item.DeepCopy())); err != nil {
					return err
				}
				continue
			}
			if err := s.client.Create(ctx, pvcObj); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}

		job := &batchv1.Job{}
		err = s.client.Get(ctx, types.NamespacedName{Namespace: s.namespace, Name: jobName}, job)
		if err != nil && apierrors.IsNotFound(err) {
			if err := s.client.Create(ctx, s.buildBenchmarkJob(&resolved, jobName, pvcName)); err != nil {
				return err
			}
			now := metav1.Now()
			current.Status = inventoryv1alpha1.OpenNebulaDatastoreBenchmarkRunStatus{
				ObservedGeneration: resolved.Generation,
				Phase:              inventoryv1alpha1.ValidationPhaseRunning,
				DatastoreID:        resolved.Spec.DatastoreID,
				DatastoreName:      ds.Name,
				Backend:            normalizeDatastoreBackend(ds),
				JobName:            jobName,
				PVCName:            pvcName,
				StartedAt:          &now,
				Message:            "benchmark job created",
			}
			if err := s.client.Status().Patch(ctx, current, ctrlclient.MergeFrom(item.DeepCopy())); err != nil {
				return err
			}
			continue
		} else if err != nil {
			return err
		}

		nextStatus := current.Status
		nextStatus.ObservedGeneration = resolved.Generation
		nextStatus.DatastoreID = resolved.Spec.DatastoreID
		nextStatus.DatastoreName = ds.Name
		nextStatus.Backend = normalizeDatastoreBackend(ds)
		nextStatus.JobName = jobName
		nextStatus.PVCName = pvcName
		if nextStatus.StartedAt == nil {
			now := metav1.Now()
			nextStatus.StartedAt = &now
		}
		nextStatus.Phase = inventoryv1alpha1.ValidationPhaseRunning
		nextStatus.Message = "benchmark job is running"
		nextStatus.Summary = "-"

		if job.Status.Succeeded > 0 {
			now := metav1.Now()
			nextStatus.CompletedAt = &now
			nextStatus.Phase = inventoryv1alpha1.ValidationPhaseSucceeded
			nextStatus.Message = "benchmark completed successfully"
			if result, err := s.parseValidationJobLogs(ctx, job); err == nil {
				nextStatus.Result = result
				nextStatus.Summary = validationMetricsDisplay(inventoryv1alpha1.OpenNebulaDatastoreValidationStatus{
					Phase:  inventoryv1alpha1.ValidationPhaseSucceeded,
					Result: result,
				})
				if thresholdErr := benchmarkThresholdFailure(result, resolved.Spec.Thresholds); thresholdErr != "" {
					nextStatus.Phase = inventoryv1alpha1.ValidationPhaseFailed
					nextStatus.Message = thresholdErr
				}
			} else {
				nextStatus.FailureLog = s.validationJobLogExcerpt(ctx, job)
			}
			current.Status = nextStatus
			if err := s.client.Status().Patch(ctx, current, ctrlclient.MergeFrom(item.DeepCopy())); err != nil {
				return err
			}
			_ = s.cleanupValidationResources(ctx, jobName, pvcName)
			continue
		}
		if job.Status.Failed > 0 {
			now := metav1.Now()
			nextStatus.CompletedAt = &now
			nextStatus.Phase = inventoryv1alpha1.ValidationPhaseFailed
			nextStatus.Message = "benchmark failed"
			nextStatus.FailureLog = s.validationJobLogExcerpt(ctx, job)
			current.Status = nextStatus
			if err := s.client.Status().Patch(ctx, current, ctrlclient.MergeFrom(item.DeepCopy())); err != nil {
				return err
			}
			_ = s.cleanupValidationResources(ctx, jobName, pvcName)
			continue
		}

		current.Status = nextStatus
		if err := s.client.Status().Patch(ctx, current, ctrlclient.MergeFrom(item.DeepCopy())); err != nil {
			return err
		}
	}
	return nil
}

func (s *Syncer) expandAutoBenchmarkRun(ctx context.Context, item *inventoryv1alpha1.OpenNebulaDatastoreBenchmarkRun, scs []storagev1.StorageClass) error {
	if item.Spec.DatastoreID <= 0 {
		current := item.DeepCopy()
		current.Status = inventoryv1alpha1.OpenNebulaDatastoreBenchmarkRunStatus{
			ObservedGeneration: item.Generation,
			Phase:              inventoryv1alpha1.ValidationPhaseFailed,
			DatastoreID:        item.Spec.DatastoreID,
			Message:            "auto benchmark runs require spec.datastoreID",
		}
		return s.client.Status().Patch(ctx, current, ctrlclient.MergeFrom(item.DeepCopy()))
	}
	resolved := item.DeepCopy()
	if strings.TrimSpace(resolved.Spec.StorageClassName) == "" {
		resolved.Spec.StorageClassName = defaultBenchmarkStorageClassName(scs, resolved.Spec.DatastoreID)
	}
	if strings.TrimSpace(resolved.Spec.Size) == "" {
		resolved.Spec.Size = "1Gi"
	}
	name := benchmarkRunObjectName(resolved.Spec.DatastoreID, time.Now().UTC())
	created := &inventoryv1alpha1.OpenNebulaDatastoreBenchmarkRun{
		TypeMeta: metav1.TypeMeta{
			APIVersion: inventoryv1alpha1.SchemeGroupVersion.String(),
			Kind:       "OpenNebulaDatastoreBenchmarkRun",
		},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       resolved.Spec,
	}
	if err := s.client.Create(ctx, created); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return s.client.Delete(ctx, item)
}

func (s *Syncer) resolveBenchmarkRun(item inventoryv1alpha1.OpenNebulaDatastoreBenchmarkRun, ds datastoreSchema.Datastore, scs []storagev1.StorageClass) inventoryv1alpha1.OpenNebulaDatastoreBenchmarkRun {
	resolved := *item.DeepCopy()
	applyBenchmarkProfileDefaults(&resolved)
	if strings.TrimSpace(resolved.Spec.StorageClassName) == "" {
		resolved.Spec.StorageClassName = defaultBenchmarkStorageClassName(scs, resolved.Spec.DatastoreID)
	}
	if strings.TrimSpace(resolved.Spec.Size) == "" {
		resolved.Spec.Size = "1Gi"
	}
	if len(resolved.Spec.AccessModes) == 0 {
		resolved.Spec.AccessModes = defaultPVCAccessModes(normalizeDatastoreBackend(ds))
	}
	if strings.TrimSpace(resolved.Spec.Image) == "" {
		resolved.Spec.Image = s.defaultValidationImage
	}
	if resolved.Spec.ImagePullPolicy == "" {
		resolved.Spec.ImagePullPolicy = defaultValidationImagePullPolicy
	}
	if resolved.Spec.ActiveDeadlineSeconds == nil {
		resolved.Spec.ActiveDeadlineSeconds = int64Ptr(defaultValidationActiveDeadlineSeconds)
	}
	return resolved
}

func applyBenchmarkProfileDefaults(run *inventoryv1alpha1.OpenNebulaDatastoreBenchmarkRun) {
	if run == nil {
		return
	}
	switch strings.ToLower(strings.TrimSpace(run.Spec.Profile)) {
	case "latency":
		if strings.TrimSpace(run.Spec.Size) == "" {
			run.Spec.Size = "4Gi"
		}
		if len(run.Spec.FioArgs) == 0 {
			run.Spec.FioArgs = []string{"--name=latency", "--filename=/data/validation.bin", "--rw=randread", "--bs=4k", "--size=256Mi", "--iodepth=1", "--runtime=60", "--time_based"}
		}
	case "throughput":
		if strings.TrimSpace(run.Spec.Size) == "" {
			run.Spec.Size = "8Gi"
		}
		if len(run.Spec.FioArgs) == 0 {
			run.Spec.FioArgs = []string{"--name=throughput", "--filename=/data/validation.bin", "--rw=read", "--bs=1M", "--size=1Gi", "--iodepth=32", "--runtime=60", "--time_based"}
		}
	case "metadata":
		if strings.TrimSpace(run.Spec.Size) == "" {
			run.Spec.Size = "4Gi"
		}
		if len(run.Spec.FioArgs) == 0 {
			run.Spec.FioArgs = []string{"--name=metadata", "--directory=/data", "--rw=randrw", "--bs=4k", "--nrfiles=1024", "--filesize=64k", "--iodepth=16", "--runtime=60", "--time_based"}
		}
	case "rwx-contention":
		if strings.TrimSpace(run.Spec.Size) == "" {
			run.Spec.Size = "4Gi"
		}
		if len(run.Spec.AccessModes) == 0 {
			run.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}
		}
		if len(run.Spec.FioArgs) == 0 {
			run.Spec.FioArgs = []string{"--name=rwx-contention", "--filename=/data/validation.bin", "--rw=randrw", "--bs=4k", "--size=512Mi", "--iodepth=16", "--runtime=60", "--time_based"}
		}
	case "smoke":
		if strings.TrimSpace(run.Spec.Size) == "" {
			run.Spec.Size = "1Gi"
		}
	}
}

func (s *Syncer) loadLatestBenchmarkResults(ctx context.Context) (map[int]*latestBenchmarkResult, error) {
	var runList inventoryv1alpha1.OpenNebulaDatastoreBenchmarkRunList
	if err := s.apiReader.List(ctx, &runList); err != nil {
		return nil, err
	}
	results := make(map[int]*latestBenchmarkResult)
	for i := range runList.Items {
		run := runList.Items[i]
		if run.Status.Phase != inventoryv1alpha1.ValidationPhaseSucceeded {
			continue
		}
		existing, ok := results[run.Spec.DatastoreID]
		if !ok || benchmarkCompletedAfter(run.Status.CompletedAt, existing.Status.CompletedAt) {
			statusCopy := run.Status
			results[run.Spec.DatastoreID] = &latestBenchmarkResult{
				RunName: run.Name,
				Status:  statusCopy,
			}
		}
	}
	return results, nil
}

func (s *Syncer) reconcileValidation(ctx context.Context, ds *inventoryv1alpha1.OpenNebulaDatastore) error {
	if ds.Spec.Validation.Mode != inventoryv1alpha1.DatastoreValidationModeManual {
		return nil
	}
	if strings.TrimSpace(ds.Spec.Validation.RunNonce) == "" {
		return nil
	}
	if ds.Spec.Validation.RunNonce == ds.Status.Validation.LastRunNonce && ds.Status.Validation.Phase != inventoryv1alpha1.ValidationPhaseRunning {
		return nil
	}

	jobName := validationResourceName(validationJobPrefix, ds.Name, ds.Spec.Validation.RunNonce)
	pvcName := validationResourceName(validationPVCPrefix, ds.Name, ds.Spec.Validation.RunNonce)
	if ds.Spec.Validation.RunNonce == ds.Status.Validation.LastRunNonce && isTerminalValidationPhase(ds.Status.Validation.Phase) {
		return s.cleanupValidationResources(ctx, jobName, pvcName)
	}
	pvc := &corev1.PersistentVolumeClaim{}
	err := s.client.Get(ctx, types.NamespacedName{Namespace: s.namespace, Name: pvcName}, pvc)
	if err != nil && apierrors.IsNotFound(err) {
		pvcObj, buildErr := s.buildValidationPVC(ds, pvcName)
		if buildErr != nil {
			now := metav1.Now()
			ds.Status.Validation.LastCompletedTime = &now
			ds.Status.Validation.LastRunNonce = ds.Spec.Validation.RunNonce
			ds.Status.Validation.Phase = inventoryv1alpha1.ValidationPhaseFailed
			ds.Status.Validation.JobName = jobName
			ds.Status.Validation.Message = buildErr.Error()
			return s.client.Status().Update(ctx, ds)
		}
		if err := s.client.Create(ctx, pvcObj); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	job := &batchv1.Job{}
	err = s.client.Get(ctx, types.NamespacedName{Namespace: s.namespace, Name: jobName}, job)
	if err != nil && apierrors.IsNotFound(err) {
		if err := s.client.Create(ctx, s.buildValidationJob(ds, jobName, pvcName)); err != nil {
			return err
		}
		now := metav1.Now()
		ds.Status.Validation.LastRunTime = &now
		ds.Status.Validation.LastRunNonce = ds.Spec.Validation.RunNonce
		ds.Status.Validation.Phase = inventoryv1alpha1.ValidationPhaseRunning
		ds.Status.Validation.JobName = jobName
		ds.Status.Validation.Message = "validation job created"
		return s.client.Status().Update(ctx, ds)
	} else if err != nil {
		return err
	}

	if job.Status.Succeeded > 0 {
		now := metav1.Now()
		ds.Status.Validation.LastCompletedTime = &now
		ds.Status.Validation.LastRunNonce = ds.Spec.Validation.RunNonce
		ds.Status.Validation.Phase = inventoryv1alpha1.ValidationPhaseSucceeded
		ds.Status.Validation.Message = "validation completed successfully"
		if result, err := s.parseValidationJobLogs(ctx, job); err == nil {
			ds.Status.Validation.Result = result
		}
		if err := s.client.Status().Update(ctx, ds); err != nil {
			return err
		}
		return s.cleanupValidationResources(ctx, jobName, pvcName)
	}
	if job.Status.Failed > 0 {
		now := metav1.Now()
		ds.Status.Validation.LastCompletedTime = &now
		ds.Status.Validation.LastRunNonce = ds.Spec.Validation.RunNonce
		ds.Status.Validation.Phase = inventoryv1alpha1.ValidationPhaseFailed
		ds.Status.Validation.Message = "validation failed"
		if err := s.client.Status().Update(ctx, ds); err != nil {
			return err
		}
		return s.cleanupValidationResources(ctx, jobName, pvcName)
	}

	ds.Status.Validation.Phase = inventoryv1alpha1.ValidationPhaseRunning
	ds.Status.Validation.JobName = jobName
	return s.client.Status().Update(ctx, ds)
}

func (s *Syncer) buildValidationPVC(ds *inventoryv1alpha1.OpenNebulaDatastore, pvcName string) (*corev1.PersistentVolumeClaim, error) {
	size := ds.Spec.Validation.JobTemplate.Size
	if strings.TrimSpace(size) == "" {
		size = "4Gi"
	}
	accessModes, err := normalizePVCAccessModes(ds.Spec.Validation.JobTemplate.AccessModes, validationBackend(ds))
	if err != nil {
		return nil, err
	}
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: s.namespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      accessModes,
			StorageClassName: stringPtr(ds.Spec.Validation.JobTemplate.StorageClassName),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resourceMustParse(size),
				},
			},
		},
	}, nil
}

func (s *Syncer) buildBenchmarkPVC(run *inventoryv1alpha1.OpenNebulaDatastoreBenchmarkRun, ds datastoreSchema.Datastore, pvcName string) (*corev1.PersistentVolumeClaim, error) {
	size := run.Spec.Size
	if strings.TrimSpace(size) == "" {
		size = "1Gi"
	}
	accessModes, err := normalizePVCAccessModes(run.Spec.AccessModes, normalizeDatastoreBackend(ds))
	if err != nil {
		return nil, err
	}
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: s.namespace,
			Labels:    benchmarkResourceLabels(run),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      accessModes,
			StorageClassName: stringPtr(run.Spec.StorageClassName),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resourceMustParse(size),
				},
			},
		},
	}, nil
}

func (s *Syncer) buildValidationJob(ds *inventoryv1alpha1.OpenNebulaDatastore, jobName, pvcName string) *batchv1.Job {
	image := ds.Spec.Validation.JobTemplate.Image
	if strings.TrimSpace(image) == "" {
		image = s.defaultValidationImage
	}
	policy := ds.Spec.Validation.JobTemplate.ImagePullPolicy
	if policy == "" {
		policy = defaultValidationImagePullPolicy
	}
	ttl := ds.Spec.Validation.JobTemplate.TTLSecondsAfterFinished
	command := []string{"/bin/sh", "-c"}
	args := []string{buildValidationCommand(ds.Spec.Validation.JobTemplate.FioArgs)}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: s.namespace,
		},
		Spec: batchv1.JobSpec{
			ActiveDeadlineSeconds:   validationActiveDeadlineSeconds(ds.Spec.Validation.JobTemplate.ActiveDeadlineSeconds),
			TTLSecondsAfterFinished: ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					NodeSelector:  ds.Spec.Validation.JobTemplate.NodeSelector,
					Tolerations:   ds.Spec.Validation.JobTemplate.Tolerations,
					Containers: []corev1.Container{{
						Name:            "fio",
						Image:           image,
						ImagePullPolicy: policy,
						Command:         command,
						Args:            args,
						Resources:       ds.Spec.Validation.JobTemplate.Resources,
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "data",
							MountPath: "/data",
						}},
					}},
					Volumes: []corev1.Volume{{
						Name: "data",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: pvcName,
							},
						},
					}},
				},
			},
		},
	}
}

func (s *Syncer) buildBenchmarkJob(run *inventoryv1alpha1.OpenNebulaDatastoreBenchmarkRun, jobName, pvcName string) *batchv1.Job {
	image := run.Spec.Image
	if strings.TrimSpace(image) == "" {
		image = s.defaultValidationImage
	}
	policy := run.Spec.ImagePullPolicy
	if policy == "" {
		policy = defaultValidationImagePullPolicy
	}
	ttl := run.Spec.TTLSecondsAfterFinished
	command := []string{"/bin/sh", "-c"}
	args := []string{buildValidationCommand(run.Spec.FioArgs)}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: s.namespace,
			Labels:    benchmarkResourceLabels(run),
		},
		Spec: batchv1.JobSpec{
			ActiveDeadlineSeconds:   validationActiveDeadlineSeconds(run.Spec.ActiveDeadlineSeconds),
			TTLSecondsAfterFinished: ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: benchmarkResourceLabels(run),
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					NodeSelector:  run.Spec.NodeSelector,
					Tolerations:   run.Spec.Tolerations,
					Containers: []corev1.Container{{
						Name:            "fio",
						Image:           image,
						ImagePullPolicy: policy,
						Command:         command,
						Args:            args,
						Resources:       run.Spec.Resources,
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "data",
							MountPath: "/data",
						}},
					}},
					Volumes: []corev1.Volume{{
						Name: "data",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: pvcName,
							},
						},
					}},
				},
			},
		},
	}
}

func (s *Syncer) parseValidationJobLogs(ctx context.Context, job *batchv1.Job) (inventoryv1alpha1.ValidationResult, error) {
	pods, err := s.kube.CoreV1().Pods(job.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + job.Name,
	})
	if err != nil || len(pods.Items) == 0 {
		return inventoryv1alpha1.ValidationResult{}, fmt.Errorf("validation pod was not found")
	}
	req := s.kube.CoreV1().Pods(job.Namespace).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{Container: "fio"})
	stream, err := req.Stream(ctx)
	if err != nil {
		return inventoryv1alpha1.ValidationResult{}, err
	}
	defer stream.Close()
	body, err := io.ReadAll(stream)
	if err != nil {
		return inventoryv1alpha1.ValidationResult{}, err
	}
	return parseFIOResult(body)
}

func (s *Syncer) validationJobLogExcerpt(ctx context.Context, job *batchv1.Job) string {
	pods, err := s.kube.CoreV1().Pods(job.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + job.Name,
	})
	if err != nil || len(pods.Items) == 0 {
		return ""
	}
	req := s.kube.CoreV1().Pods(job.Namespace).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{Container: "fio", TailLines: int64Ptr(80)})
	stream, err := req.Stream(ctx)
	if err != nil {
		return ""
	}
	defer stream.Close()
	body, err := io.ReadAll(stream)
	if err != nil {
		return ""
	}
	trimmed := strings.TrimSpace(string(body))
	if len(trimmed) > 4000 {
		return trimmed[len(trimmed)-4000:]
	}
	return trimmed
}

func benchmarkThresholdFailure(result inventoryv1alpha1.ValidationResult, thresholds inventoryv1alpha1.BenchmarkThresholds) string {
	if thresholds.MinReadIops != nil && (result.ReadIops == nil || *result.ReadIops < *thresholds.MinReadIops) {
		return fmt.Sprintf("read IOPS below threshold: got %s, want >= %d", pointerInt64Display(result.ReadIops), *thresholds.MinReadIops)
	}
	if thresholds.MinWriteIops != nil && (result.WriteIops == nil || *result.WriteIops < *thresholds.MinWriteIops) {
		return fmt.Sprintf("write IOPS below threshold: got %s, want >= %d", pointerInt64Display(result.WriteIops), *thresholds.MinWriteIops)
	}
	if thresholds.MinReadBwBytes != nil && (result.ReadBwBytes == nil || *result.ReadBwBytes < *thresholds.MinReadBwBytes) {
		return fmt.Sprintf("read bandwidth below threshold: got %s, want >= %d", pointerInt64Display(result.ReadBwBytes), *thresholds.MinReadBwBytes)
	}
	if thresholds.MinWriteBwBytes != nil && (result.WriteBwBytes == nil || *result.WriteBwBytes < *thresholds.MinWriteBwBytes) {
		return fmt.Sprintf("write bandwidth below threshold: got %s, want >= %d", pointerInt64Display(result.WriteBwBytes), *thresholds.MinWriteBwBytes)
	}
	if thresholds.MaxLatencyP99Micros != nil && (result.LatencyP99Micros == nil || *result.LatencyP99Micros > *thresholds.MaxLatencyP99Micros) {
		return fmt.Sprintf("p99 latency above threshold: got %s, want <= %d", pointerInt64Display(result.LatencyP99Micros), *thresholds.MaxLatencyP99Micros)
	}
	return ""
}

func pointerInt64Display(value *int64) string {
	if value == nil {
		return "<missing>"
	}
	return strconv.FormatInt(*value, 10)
}

func parseFIOResult(payload []byte) (inventoryv1alpha1.ValidationResult, error) {
	trimmed := strings.TrimSpace(string(payload))
	start := strings.Index(trimmed, "{")
	end := strings.LastIndex(trimmed, "}")
	if start < 0 || end < start {
		return inventoryv1alpha1.ValidationResult{}, fmt.Errorf("fio result did not include a JSON object")
	}
	var decoded struct {
		Jobs []struct {
			Read struct {
				IOPS    float64 `json:"iops"`
				BWBytes int64   `json:"bw_bytes"`
				ClatNS  struct {
					Percentile map[string]float64 `json:"percentile"`
				} `json:"clat_ns"`
			} `json:"read"`
			Write struct {
				IOPS    float64 `json:"iops"`
				BWBytes int64   `json:"bw_bytes"`
				ClatNS  struct {
					Percentile map[string]float64 `json:"percentile"`
				} `json:"clat_ns"`
			} `json:"write"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal([]byte(trimmed[start:end+1]), &decoded); err != nil {
		return inventoryv1alpha1.ValidationResult{}, err
	}
	if len(decoded.Jobs) == 0 {
		return inventoryv1alpha1.ValidationResult{}, fmt.Errorf("fio result did not include jobs")
	}
	job := decoded.Jobs[0]
	readIops := int64(job.Read.IOPS)
	writeIops := int64(job.Write.IOPS)
	readP50 := percentileMicros(job.Read.ClatNS.Percentile, "50.000000")
	readP99 := percentileMicros(job.Read.ClatNS.Percentile, "99.000000")
	return inventoryv1alpha1.ValidationResult{
		ReadIops:         &readIops,
		WriteIops:        &writeIops,
		ReadBwBytes:      &job.Read.BWBytes,
		WriteBwBytes:     &job.Write.BWBytes,
		LatencyP50Micros: &readP50,
		LatencyP99Micros: &readP99,
	}, nil
}

func buildValidationCommand(args []string) string {
	defaultArgs := []string{
		"--name=validate",
		"--filename=/data/validation.bin",
		"--rw=randrw",
		"--bs=4k",
		"--size=256Mi",
		"--iodepth=16",
		"--runtime=30",
		"--time_based",
	}
	args = mergeValidationArgs(defaultArgs, args)
	return "set -eu; if ! command -v fio >/dev/null 2>&1; then if command -v apk >/dev/null 2>&1; then apk add --no-cache fio >/dev/null; else echo 'fio binary is unavailable and apk is not installed' >&2; exit 1; fi; fi; fio " + strings.Join(args, " ") + " --output-format=json"
}

type validationArgEntry struct {
	key    string
	tokens []string
}

func mergeValidationArgs(defaultArgs, overrideArgs []string) []string {
	if len(overrideArgs) == 0 {
		return append([]string(nil), defaultArgs...)
	}

	entries := parseValidationArgs(defaultArgs)
	for _, entry := range parseValidationArgs(overrideArgs) {
		if entry.key == "" {
			entries = append(entries, entry)
			continue
		}

		replaced := false
		for i := range entries {
			if entries[i].key == entry.key {
				entries[i] = entry
				replaced = true
				break
			}
		}
		if !replaced {
			entries = append(entries, entry)
		}
	}

	merged := make([]string, 0, len(defaultArgs)+len(overrideArgs))
	for _, entry := range entries {
		merged = append(merged, entry.tokens...)
	}
	return merged
}

func parseValidationArgs(args []string) []validationArgEntry {
	entries := make([]validationArgEntry, 0, len(args))
	for i := 0; i < len(args); i++ {
		current := strings.TrimSpace(args[i])
		if current == "" {
			continue
		}

		entry := validationArgEntry{tokens: []string{current}}
		if strings.HasPrefix(current, "--") {
			entry.key = validationArgKey(current)
			if !strings.Contains(current, "=") && i+1 < len(args) {
				next := strings.TrimSpace(args[i+1])
				if next != "" && !strings.HasPrefix(next, "--") {
					entry.tokens = append(entry.tokens, next)
					i++
				}
			}
		}

		entries = append(entries, entry)
	}
	return entries
}

func validationArgKey(arg string) string {
	if idx := strings.Index(arg, "="); idx >= 0 {
		return arg[:idx]
	}
	return arg
}

func isTerminalValidationPhase(phase string) bool {
	switch phase {
	case inventoryv1alpha1.ValidationPhaseSucceeded, inventoryv1alpha1.ValidationPhaseFailed:
		return true
	default:
		return false
	}
}

func cleanupValidationResource(ctx context.Context, client ctrlclient.Client, obj ctrlclient.Object) error {
	opts := []ctrlclient.DeleteOption{}
	if _, ok := obj.(*batchv1.Job); ok {
		opts = append(opts, ctrlclient.PropagationPolicy(metav1.DeletePropagationBackground))
	}
	if err := client.Delete(ctx, obj, opts...); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func benchmarkResourceLabels(run *inventoryv1alpha1.OpenNebulaDatastoreBenchmarkRun) map[string]string {
	labels := map[string]string{
		benchmarkLabelComponent: benchmarkComponentValue,
	}
	if run != nil {
		labels[benchmarkLabelRun] = run.Name
		if run.Spec.DatastoreID > 0 {
			labels[benchmarkLabelDatastoreID] = strconv.Itoa(run.Spec.DatastoreID)
		}
	}
	return labels
}

func hasString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func removeString(values []string, target string) []string {
	filtered := values[:0]
	for _, value := range values {
		if value == target {
			continue
		}
		filtered = append(filtered, value)
	}
	return filtered
}

func (s *Syncer) cleanupValidationResources(ctx context.Context, jobName, pvcName string) error {
	if err := cleanupValidationResource(ctx, s.client, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: s.namespace}}); err != nil {
		return err
	}
	if err := cleanupValidationResource(ctx, s.client, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: s.namespace}}); err != nil {
		return err
	}
	return nil
}

func validationBackend(ds *inventoryv1alpha1.OpenNebulaDatastore) string {
	if ds == nil {
		return ""
	}
	if trimmed := strings.TrimSpace(ds.Status.Backend); trimmed != "" {
		return trimmed
	}
	return strings.TrimSpace(ds.Spec.Discovery.ExpectedBackend)
}

func defaultPVCAccessModes(backend string) []corev1.PersistentVolumeAccessMode {
	if strings.EqualFold(strings.TrimSpace(backend), "cephfs") {
		return []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}
	}
	return []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
}

func normalizePVCAccessModes(modes []corev1.PersistentVolumeAccessMode, backend string) ([]corev1.PersistentVolumeAccessMode, error) {
	if len(modes) == 0 {
		return append([]corev1.PersistentVolumeAccessMode(nil), defaultPVCAccessModes(backend)...), nil
	}

	allowed := map[corev1.PersistentVolumeAccessMode]struct{}{
		corev1.ReadWriteOnce: {},
		corev1.ReadOnlyMany:  {},
		corev1.ReadWriteMany: {},
	}
	normalized := make([]corev1.PersistentVolumeAccessMode, 0, len(modes))
	seen := make(map[corev1.PersistentVolumeAccessMode]struct{}, len(modes))
	for _, mode := range modes {
		trimmed := corev1.PersistentVolumeAccessMode(strings.TrimSpace(string(mode)))
		if _, ok := allowed[trimmed]; !ok {
			return nil, fmt.Errorf("unsupported PVC access mode %q; allowed values are ReadWriteOnce, ReadOnlyMany, ReadWriteMany", mode)
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		normalized = append(normalized, trimmed)
	}
	return normalized, nil
}

func validationActiveDeadlineSeconds(value *int64) *int64 {
	if value != nil {
		return value
	}
	return int64Ptr(defaultValidationActiveDeadlineSeconds)
}

func benchmarkResourceName(prefix string, run inventoryv1alpha1.OpenNebulaDatastoreBenchmarkRun) string {
	return validationResourceName(prefix, run.Name, fmt.Sprintf("%d-gen-%d", run.Spec.DatastoreID, run.Generation))
}

func int64Ptr(value int64) *int64 {
	return &value
}

func datastoreObjectName(id int) string {
	return fmt.Sprintf("ds-%d", id)
}

func validationResourceName(prefix, name, nonce string) string {
	hash := sha1.Sum([]byte(name + ":" + nonce))
	suffix := hex.EncodeToString(hash[:])[:8]
	trimmed := name
	if len(trimmed) > 40 {
		trimmed = trimmed[:40]
	}
	trimmed = strings.Trim(trimmed, "-")
	return prefix + trimmed + "-" + suffix
}

func normalizeDatastore(ds datastoreSchema.Datastore) opennebula.Datastore {
	return opennebula.Datastore{
		ID:                         ds.ID,
		Name:                       ds.Name,
		Type:                       normalizeDatastoreType(ds),
		Backend:                    normalizeDatastoreBackend(ds),
		DSMad:                      strings.ToLower(strings.TrimSpace(ds.DSMad)),
		TMMad:                      strings.ToLower(strings.TrimSpace(ds.TMMad)),
		FreeBytes:                  int64(ds.FreeMB) * 1024 * 1024,
		TotalBytes:                 int64(ds.TotalMB) * 1024 * 1024,
		CompatibleSystemDatastores: compatibleSystemDatastoresForInventory(ds),
	}
}

func normalizeDatastoreBackend(ds datastoreSchema.Datastore) string {
	if backend, err := ds.Template.GetStr(sharedBackendAttr); err == nil {
		switch strings.ToLower(strings.TrimSpace(backend)) {
		case "ceph", "rbd", "ceph-rbd":
			return "ceph-rbd"
		case "cephfs":
			return "cephfs"
		}
	}
	normalized := strings.ToLower(strings.TrimSpace(ds.TMMad))
	switch normalized {
	case "ceph":
		return "ceph-rbd"
	case "cephfs":
		return "cephfs"
	default:
		return "local"
	}
}

func normalizeDatastoreType(ds datastoreSchema.Datastore) string {
	trimmed := strings.ToUpper(strings.TrimSpace(ds.Type))
	switch trimmed {
	case "0":
		return string(datastoreSchema.Image)
	case "1":
		return string(datastoreSchema.System)
	case "2":
		return string(datastoreSchema.File)
	}
	if trimmed == "" {
		return "UNKNOWN"
	}
	return trimmed
}

const unknownCapacityDisplay = "Unknown / Unknown (-)"

func datastoreDisplayName(item inventoryv1alpha1.OpenNebulaDatastore, ds inventoryv1alpha1.OpenNebulaDatastoreOpenNebulaStatus) string {
	if strings.TrimSpace(ds.Name) != "" {
		return ds.Name
	}
	if strings.TrimSpace(item.Spec.DisplayName) != "" {
		return item.Spec.DisplayName
	}
	if ds.ID != 0 {
		return datastoreObjectName(ds.ID)
	}
	return item.Name
}

func expectedBackendMismatch(expected, actual string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(expected))
	return trimmed != "" && trimmed != "unknown" && trimmed != strings.ToLower(strings.TrimSpace(actual))
}

func datastoreHealthForDisplay(totalBytes int64, expectedBackend, actualBackend string) string {
	if totalBytes <= 0 {
		return inventoryv1alpha1.DatastoreHealthInvalid
	}
	if expectedBackendMismatch(expectedBackend, actualBackend) {
		return inventoryv1alpha1.DatastoreHealthBackendMismatch
	}
	return inventoryv1alpha1.DatastoreHealthHealthy
}

func datastorePhaseForDisplay(health string, enabled bool, allowed bool, maintenanceMode bool, referencedByStorageClass bool) string {
	if health != inventoryv1alpha1.DatastoreHealthHealthy {
		return inventoryv1alpha1.DatastorePhaseUnavailable
	}
	if maintenanceMode || !enabled || !allowed {
		return inventoryv1alpha1.DatastorePhaseDisabled
	}
	if referencedByStorageClass {
		return inventoryv1alpha1.DatastorePhaseEnabled
	}
	return inventoryv1alpha1.DatastorePhaseAvailable
}

func storageClassesDisplay(values []string) string {
	if len(values) == 0 {
		return "-"
	}
	return strings.Join(values, ",")
}

func formatCapacityDisplay(freeBytes, totalBytes int64) string {
	if totalBytes <= 0 {
		return unknownCapacityDisplay
	}
	usedBytes := maxInt64(totalBytes-freeBytes, 0)
	usedPercent := int(math.Round((float64(usedBytes) / float64(totalBytes)) * 100))
	return fmt.Sprintf("%s / %s (%d%%)", humanizeBytes(freeBytes), humanizeBytes(totalBytes), usedPercent)
}

func humanizeBytes(value int64) string {
	if value <= 0 {
		return "0 KB"
	}
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	scaled := float64(value) / 1024.0
	unitIdx := 0
	for scaled >= 1024.0 && unitIdx < len(units)-1 {
		scaled /= 1024.0
		unitIdx++
	}
	return fmt.Sprintf("%d %s", int(math.Round(scaled)), units[unitIdx])
}

func validationMetricsDisplay(status inventoryv1alpha1.OpenNebulaDatastoreValidationStatus) string {
	if status.Phase != inventoryv1alpha1.ValidationPhaseSucceeded || (status.Result.ReadIops == nil && status.Result.WriteIops == nil && status.Result.LatencyP99Micros == nil) {
		return "-"
	}
	return fmt.Sprintf("R:%s W:%s p99:%s", compactIOPS(status.Result.ReadIops), compactIOPS(status.Result.WriteIops), humanizeLatency(status.Result.LatencyP99Micros))
}

func benchmarkSummaryDisplay(status inventoryv1alpha1.OpenNebulaDatastoreBenchmarkRunStatus) string {
	if status.Phase != inventoryv1alpha1.ValidationPhaseSucceeded || (status.Result.ReadIops == nil && status.Result.WriteIops == nil && status.Result.LatencyP99Micros == nil) {
		return "-"
	}
	return fmt.Sprintf("R:%s W:%s p99:%s", compactIOPS(status.Result.ReadIops), compactIOPS(status.Result.WriteIops), humanizeLatency(status.Result.LatencyP99Micros))
}

func validationEligible(ds inventoryv1alpha1.OpenNebulaDatastore, backend string) bool {
	return ds.Spec.Validation.Mode == inventoryv1alpha1.DatastoreValidationModeManual
}

func validationLastOutcome(status inventoryv1alpha1.OpenNebulaDatastoreValidationStatus) string {
	switch status.Phase {
	case inventoryv1alpha1.ValidationPhaseSucceeded:
		return "Succeeded"
	case inventoryv1alpha1.ValidationPhaseFailed:
		return "Failed"
	case inventoryv1alpha1.ValidationPhaseRunning:
		return "Running"
	default:
		return "Unknown"
	}
}

func validationPhaseDisplay(status inventoryv1alpha1.OpenNebulaDatastoreValidationStatus) string {
	if strings.TrimSpace(status.Phase) == "" {
		return inventoryv1alpha1.ValidationPhaseIdle
	}
	return status.Phase
}

func validationAgeDisplay(status inventoryv1alpha1.OpenNebulaDatastoreValidationStatus) string {
	if status.LastCompletedTime != nil {
		return humanizeDurationSince(status.LastCompletedTime.Time)
	}
	if status.LastRunTime != nil {
		return humanizeDurationSince(status.LastRunTime.Time)
	}
	return "-"
}

func validationSummaryDisplay(status inventoryv1alpha1.OpenNebulaDatastoreValidationStatus) string {
	metrics := validationMetricsDisplay(status)
	if metrics != "-" {
		return metrics
	}
	switch status.Phase {
	case inventoryv1alpha1.ValidationPhaseRunning:
		return "Running"
	case inventoryv1alpha1.ValidationPhaseFailed:
		if strings.TrimSpace(status.Message) != "" {
			return status.Message
		}
		return "Failed"
	default:
		return "-"
	}
}

func benchmarkCompletedAfter(left, right *metav1.Time) bool {
	if left == nil {
		return false
	}
	if right == nil {
		return true
	}
	return left.Time.After(right.Time)
}

func benchmarkRunObjectName(datastoreID int, ts time.Time) string {
	return fmt.Sprintf("ds-%d-%s", datastoreID, ts.UTC().Format("20060102-150405"))
}

func defaultBenchmarkStorageClassName(scs []storagev1.StorageClass, datastoreID int) string {
	names := make([]string, 0)
	for _, sc := range scs {
		if storageClassUsesDatastore(sc, datastoreID) {
			names = append(names, sc.Name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

func humanizeDurationSince(ts time.Time) string {
	if ts.IsZero() {
		return "-"
	}
	d := time.Since(ts)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Round(time.Second).Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Round(time.Minute).Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Round(time.Hour).Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Round(24*time.Hour).Hours()/24))
	}
}

func compactIOPS(value *int64) string {
	if value == nil {
		return "-"
	}
	v := float64(*value)
	switch {
	case v >= 1_000_000:
		return fmt.Sprintf("%dM", int(math.Round(v/1_000_000)))
	case v >= 1_000:
		return fmt.Sprintf("%dk", int(math.Round(v/1_000)))
	default:
		return fmt.Sprintf("%d", *value)
	}
}

func humanizeLatency(micros *int64) string {
	if micros == nil {
		return "-"
	}
	switch {
	case *micros < 1_000:
		return fmt.Sprintf("%dµs", *micros)
	case *micros < 1_000_000:
		return fmt.Sprintf("%dms", int(math.Round(float64(*micros)/1_000.0)))
	default:
		return fmt.Sprintf("%ds", int(math.Round(float64(*micros)/1_000_000.0)))
	}
}

func compatibleSystemDatastoresForInventory(ds datastoreSchema.Datastore) []int {
	values := make([]int, 0)
	compatibleRaw, _ := ds.Template.GetStr("COMPATIBLE_SYS_DS")
	for _, candidate := range strings.Split(strings.TrimSpace(compatibleRaw), ",") {
		trimmed := strings.TrimSpace(candidate)
		if trimmed == "" {
			continue
		}
		id, err := strconv.Atoi(trimmed)
		if err != nil {
			continue
		}
		values = append(values, id)
	}
	return values
}

func datastoreStateString(ds datastoreSchema.Datastore) string {
	state, err := ds.State()
	if err != nil {
		return "unknown"
	}
	return state.String()
}

func datastoreIDFromPV(pv corev1.PersistentVolume) (int, bool) {
	if pv.Annotations == nil {
		return 0, false
	}
	value := strings.TrimSpace(pv.Annotations[annotationDatastoreID])
	if value == "" {
		return 0, false
	}
	id, err := strconv.Atoi(value)
	return id, err == nil
}

func storageClassUsesDatastore(sc storagev1.StorageClass, datastoreID int) bool {
	identifiers := strings.Split(sc.Parameters["datastoreIDs"], ",")
	for _, identifier := range identifiers {
		id, err := strconv.Atoi(strings.TrimSpace(identifier))
		if err == nil && id == datastoreID {
			return true
		}
	}
	return false
}

func datastoreConditions(item inventoryv1alpha1.OpenNebulaDatastore, normalized opennebula.Datastore, status inventoryv1alpha1.OpenNebulaDatastoreStatus, benchmark *latestBenchmarkResult) []metav1.Condition {
	provisioningAllowed := item.Spec.Enabled && item.Spec.Discovery.Allowed && !item.Spec.MaintenanceMode
	provisioningReason := "ProvisioningEnabled"
	provisioningMessage := "Datastore accepts new provisioning requests"
	switch {
	case item.Spec.MaintenanceMode:
		provisioningReason = "ProvisioningDisabled"
		provisioningMessage = maintenanceMessage(item.Spec.MaintenanceMessage)
	case !item.Spec.Enabled:
		provisioningReason = "ProvisioningDisabled"
		provisioningMessage = "Datastore provisioning is disabled"
	case !item.Spec.Discovery.Allowed:
		provisioningReason = "DiscoveryDisallowed"
		provisioningMessage = "Datastore is excluded from CSI provisioning"
	}

	backendMatch := !expectedBackendMismatch(item.Spec.Discovery.ExpectedBackend, normalized.Backend)
	backendReason := boolReason(backendMatch, "DatastoreFound", "BackendMismatch")
	backendMessage := "Datastore backend matches expected backend"
	if !backendMatch {
		backendMessage = fmt.Sprintf("Expected backend %q, found %q", item.Spec.Discovery.ExpectedBackend, normalized.Backend)
	}

	capacityValid := status.Capacity.TotalBytes > 0
	validationHealthy := status.Validation.Phase != inventoryv1alpha1.ValidationPhaseFailed
	validationReason := "ValidationPending"
	validationMessage := "No successful validation result recorded"
	switch status.Validation.Phase {
	case inventoryv1alpha1.ValidationPhaseSucceeded:
		validationReason = "ValidationSucceeded"
		validationMessage = "Validation completed successfully"
	case inventoryv1alpha1.ValidationPhaseFailed:
		validationReason = "ValidationFailed"
		validationMessage = strings.TrimSpace(status.Validation.Message)
		if validationMessage == "" {
			validationMessage = "Validation failed"
		}
	case inventoryv1alpha1.ValidationPhaseRunning:
		validationReason = "ValidationPending"
		validationMessage = "Validation job is running"
	}

	referenceReason := "NoStorageClassReference"
	referenceMessage := "No StorageClass references this datastore"
	if status.ReferencedByStorageClass {
		referenceReason = "StorageClassReferenced"
		referenceMessage = fmt.Sprintf("%d StorageClass reference(s) found", status.ReferenceCount)
	}

	provisioningFeasible := provisioningAllowed && backendMatch && capacityValid
	provisioningFeasibleReason := boolReason(provisioningFeasible, "ProvisioningFeasible", "ProvisioningBlocked")
	provisioningFeasibleMessage := "Datastore is eligible for provisioning"
	if !provisioningFeasible {
		provisioningFeasibleMessage = "Datastore is not currently eligible for provisioning; inspect ProvisioningEnabled, BackendMatch, and CapacityReported"
	}

	attachFeasible := true
	attachReason := "AttachFeasible"
	attachMessage := "Attach feasibility checks passed"
	if normalized.Backend == "ceph-rbd" || normalized.Backend == "ceph" {
		if len(normalized.CompatibleSystemDatastores) == 0 {
			attachFeasible = false
			attachReason = "NoCompatibleSystemDatastores"
			attachMessage = "Ceph RBD datastore does not report compatible system datastores"
		} else {
			attachMessage = fmt.Sprintf("Ceph RBD datastore reports compatible system datastores %v", normalized.CompatibleSystemDatastores)
		}
	}

	secretValid := true
	secretReason := "SecretsNotRequired"
	secretMessage := "No datastore-level secret references are required"
	if normalized.Backend == "cephfs" {
		secretReason = "StorageClassSecretsEvaluated"
		secretMessage = "CephFS StorageClass secret references were evaluated"
		for _, detail := range status.StorageClassDetails {
			for _, warning := range detail.Warnings {
				if strings.Contains(strings.ToLower(warning), "secret") {
					secretValid = false
					secretReason = "SecretReferencesIncomplete"
					secretMessage = warning
					break
				}
			}
			if !secretValid {
				break
			}
		}
	}

	benchmarkSucceeded := benchmark != nil && benchmark.Status.Phase == inventoryv1alpha1.ValidationPhaseSucceeded
	benchmarkReason := boolReason(benchmarkSucceeded, "BenchmarkSucceeded", "BenchmarkMissing")
	benchmarkMessage := "No successful benchmark result recorded"
	if benchmarkSucceeded {
		benchmarkMessage = benchmarkSummaryDisplay(benchmark.Status)
	}

	return []metav1.Condition{
		newCondition("Discovered", metav1.ConditionTrue, "DatastoreFound", "Datastore was discovered in OpenNebula"),
		newCondition("ProvisioningEnabled", boolCondition(provisioningAllowed), provisioningReason, provisioningMessage),
		newCondition("ProvisioningFeasible", boolCondition(provisioningFeasible), provisioningFeasibleReason, provisioningFeasibleMessage),
		newCondition("BackendMatch", boolCondition(backendMatch), backendReason, backendMessage),
		newCondition("CapacityReported", boolCondition(capacityValid), boolReason(capacityValid, "CapacityObserved", "CapacityInvalid"), boolMessage(capacityValid, "Datastore capacity reported from OpenNebula", "Datastore capacity is invalid or unavailable")),
		newCondition("AttachFeasible", boolCondition(attachFeasible), attachReason, attachMessage),
		newCondition("NodeSystemDatastoreCompatible", boolCondition(attachFeasible), attachReason, attachMessage),
		newCondition("CephSecretsValid", boolCondition(secretValid), secretReason, secretMessage),
		newCondition("ValidationHealthy", boolCondition(validationHealthy), validationReason, validationMessage),
		newCondition("RecentBenchmarkSucceeded", boolCondition(benchmarkSucceeded), benchmarkReason, benchmarkMessage),
		newCondition("ReferencedByStorageClass", boolCondition(status.ReferencedByStorageClass), referenceReason, referenceMessage),
	}
}

func maintenanceMessage(message string) string {
	if trimmed := strings.TrimSpace(message); trimmed != "" {
		return trimmed
	}
	return "Datastore is in maintenance mode"
}

func buildStorageClassDetails(scs []storagev1.StorageClass, ds opennebula.Datastore) []inventoryv1alpha1.StorageClassDetail {
	details := make([]inventoryv1alpha1.StorageClassDetail, 0)
	for _, sc := range scs {
		if !storageClassUsesDatastore(sc, ds.ID) {
			continue
		}
		mode := string(storagev1.VolumeBindingImmediate)
		if sc.VolumeBindingMode != nil {
			mode = string(*sc.VolumeBindingMode)
		}
		allowExpansion := false
		if sc.AllowVolumeExpansion != nil {
			allowExpansion = *sc.AllowVolumeExpansion
		}
		detail := inventoryv1alpha1.StorageClassDetail{
			Name:                 sc.Name,
			VolumeBindingMode:    mode,
			AllowVolumeExpansion: allowExpansion,
			BackendCompatible:    storageClassBackendCompatible(sc, ds.Backend),
		}
		if ds.Backend == "local" && mode == string(storagev1.VolumeBindingImmediate) {
			detail.Warnings = append(detail.Warnings, "local datastore uses Immediate binding")
		}
		if !allowExpansion {
			detail.Warnings = append(detail.Warnings, "volume expansion is disabled")
		}
		if ds.Backend == "cephfs" {
			if !hasCompleteCSISecretRef(sc.Parameters, "csi.storage.k8s.io/provisioner-secret-name", "csi.storage.k8s.io/provisioner-secret-namespace") {
				detail.Warnings = append(detail.Warnings, "cephfs provisioner secret references are incomplete")
			}
			if !hasCompleteCSISecretRef(sc.Parameters, "csi.storage.k8s.io/node-stage-secret-name", "csi.storage.k8s.io/node-stage-secret-namespace") {
				detail.Warnings = append(detail.Warnings, "cephfs node-stage secret references are incomplete")
			}
			if allowExpansion && !hasCompleteCSISecretRef(sc.Parameters, "csi.storage.k8s.io/controller-expand-secret-name", "csi.storage.k8s.io/controller-expand-secret-namespace") {
				detail.Warnings = append(detail.Warnings, "cephfs controller-expand secret references are incomplete")
			}
		}
		if !detail.BackendCompatible {
			detail.Warnings = append(detail.Warnings, "storage class backend does not match datastore backend")
		}
		details = append(details, detail)
	}
	sort.Slice(details, func(i, j int) bool {
		return details[i].Name < details[j].Name
	})
	return details
}

func storageClassBackendCompatible(sc storagev1.StorageClass, backend string) bool {
	driver := strings.ToLower(strings.TrimSpace(sc.Parameters["driver"]))
	switch backend {
	case "cephfs":
		if driver == "ceph" || driver == "rbd" {
			return false
		}
		if driver == "cephfs" || storageClassSignalsCephFS(sc) {
			return true
		}
		return true
	case "ceph-rbd":
		if driver == "cephfs" || storageClassSignalsCephFS(sc) {
			return false
		}
		if driver == "ceph" || driver == "rbd" {
			return true
		}
		return true
	case "local":
		if driver == "cephfs" || storageClassSignalsCephFS(sc) {
			return false
		}
		return true
	}
	return true
}

func storageClassSignalsCephFS(sc storagev1.StorageClass) bool {
	if strings.TrimSpace(sc.Parameters["sharedFilesystemPath"]) != "" || strings.TrimSpace(sc.Parameters["sharedFilesystemSubvolumeGroup"]) != "" {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(sc.Parameters["driver"]), "cephfs") {
		return true
	}
	return false
}

func hasCompleteCSISecretRef(params map[string]string, nameKey, namespaceKey string) bool {
	return strings.TrimSpace(params[nameKey]) != "" && strings.TrimSpace(params[namespaceKey]) != ""
}

func nodeDisplayState(enabled, inCooldown, ready bool) string {
	switch {
	case !enabled:
		return "Disabled"
	case inCooldown:
		return "HotplugCooldown"
	case !ready:
		return "VMNotReady"
	default:
		return "Ready"
	}
}

func attachedVolumeHandlesDisplay(handles []string) string {
	if len(handles) == 0 {
		return "-"
	}
	if len(handles) <= 3 {
		return strings.Join(handles, ",")
	}
	return fmt.Sprintf("%s (+%d more)", strings.Join(handles[:3], ","), len(handles)-3)
}

func datastoreNameMap(pool *datastoreSchema.Pool) map[int]string {
	names := make(map[int]string)
	if pool == nil {
		return names
	}
	for _, ds := range pool.Datastores {
		names[ds.ID] = ds.Name
	}
	return names
}

func filteredNodeLabels(labels map[string]string) map[string]string {
	filtered := make(map[string]string)
	for key, value := range labels {
		switch {
		case strings.HasPrefix(key, "kubernetes.io/"),
			strings.HasPrefix(key, "node.kubernetes.io/"),
			strings.HasPrefix(key, "topology.kubernetes.io/"),
			strings.HasPrefix(key, "topology.opennebula.sparkaiur.io/"):
			filtered[key] = value
		}
	}
	return filtered
}

func convertNodeConditions(conditions []corev1.NodeCondition) []inventoryv1alpha1.NodeConditionSummary {
	converted := make([]inventoryv1alpha1.NodeConditionSummary, 0, len(conditions))
	for _, condition := range conditions {
		converted = append(converted, inventoryv1alpha1.NodeConditionSummary{
			Type:               string(condition.Type),
			Status:             string(condition.Status),
			Reason:             condition.Reason,
			Message:            condition.Message,
			LastTransitionTime: condition.LastTransitionTime,
		})
	}
	return converted
}

func newCondition(conditionType string, status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}
}

func boolCondition(value bool) metav1.ConditionStatus {
	if value {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func boolReason(value bool, whenTrue, whenFalse string) string {
	if value {
		return whenTrue
	}
	return whenFalse
}

func boolMessage(value bool, whenTrue, whenFalse string) string {
	if value {
		return whenTrue
	}
	return whenFalse
}

func sortedKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func latestHistoryDatastoreIDForInventory(vmInfo *vmSchema.VM) (int, error) {
	var latest *vmSchema.HistoryRecord
	for idx := range vmInfo.HistoryRecords {
		record := &vmInfo.HistoryRecords[idx]
		if record.DSID < 0 {
			continue
		}
		if latest == nil || record.SEQ > latest.SEQ {
			latest = record
		}
	}
	if latest == nil {
		return 0, fmt.Errorf("vm %d has no datastore history", vmInfo.ID)
	}
	return latest.DSID, nil
}

func persistentDiskIDFromVM(vmInfo *vmSchema.VM, volumeHandle string) int {
	for _, disk := range vmInfo.Template.GetDisks() {
		imageID, err := disk.GetI(shared.ImageID)
		if err != nil {
			continue
		}
		if volumeHandle != fmt.Sprintf("pvc-%d", imageID) {
			// best effort only; most CSI volume handles are PVC IDs rather than image IDs
			continue
		}
		return imageID
	}
	return 0
}

func percentileMicros(values map[string]float64, key string) int64 {
	if values == nil {
		return 0
	}
	return int64(values[key] / 1000.0)
}

func stringPtr(value string) *string {
	return &value
}

func resourceMustParse(value string) resource.Quantity {
	return resource.MustParse(value)
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func (s *Syncer) loadHotplugStateMap(ctx context.Context) (map[string]opennebula.HotplugCooldownState, error) {
	cm, err := s.kube.CoreV1().ConfigMaps(s.namespace).Get(ctx, hotplugStateConfigMapName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return map[string]opennebula.HotplugCooldownState{}, nil
		}
		return nil, err
	}
	result := make(map[string]opennebula.HotplugCooldownState, len(cm.Data))
	for node, raw := range cm.Data {
		if node == "maintainenceMode" || node == "maintenanceMode" || node == "maintainenceReady" || node == "maintenanceReady" {
			continue
		}
		var state opennebula.HotplugCooldownState
		if err := json.Unmarshal([]byte(raw), &state); err != nil {
			continue
		}
		if !state.PauseUntilReady && !state.ExpiresAt.IsZero() && time.Now().After(state.ExpiresAt) {
			continue
		}
		result[node] = state
	}
	return result, nil
}

func (s *Syncer) loadHotplugObservationMap(ctx context.Context) (map[string]opennebula.HotplugObservation, error) {
	cm, err := s.kube.CoreV1().ConfigMaps(s.namespace).Get(ctx, hotplugDiagnosticsConfigMapName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return map[string]opennebula.HotplugObservation{}, nil
		}
		return nil, err
	}
	result := make(map[string]opennebula.HotplugObservation, len(cm.Data))
	for node, raw := range cm.Data {
		var observation opennebula.HotplugObservation
		if err := json.Unmarshal([]byte(raw), &observation); err != nil {
			continue
		}
		result[node] = observation
	}
	return result, nil
}

func (s *Syncer) persistHotplugObservation(ctx context.Context, observation opennebula.HotplugObservation) {
	if s == nil || s.kube == nil || strings.TrimSpace(observation.Node) == "" {
		return
	}
	payload, err := json.Marshal(observation)
	if err != nil {
		return
	}
	cmClient := s.kube.CoreV1().ConfigMaps(s.namespace)
	current, err := cmClient.Get(ctx, hotplugDiagnosticsConfigMapName, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return
		}
		_, _ = cmClient.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: hotplugDiagnosticsConfigMapName, Namespace: s.namespace},
			Data:       map[string]string{observation.Node: string(payload)},
		}, metav1.CreateOptions{})
		return
	}
	if current.Data == nil {
		current.Data = map[string]string{}
	}
	current.Data[observation.Node] = string(payload)
	_, _ = cmClient.Update(ctx, current, metav1.UpdateOptions{})
}

func hotplugObservationPtr(values map[string]opennebula.HotplugObservation, node string) *opennebula.HotplugObservation {
	if len(values) == 0 {
		return nil
	}
	value, ok := values[node]
	if !ok {
		return nil
	}
	return &value
}

func convertHotplugDiagnosisStatus(diagnosis opennebula.HotplugDiagnosis) inventoryv1alpha1.OpenNebulaNodeHotplugDiagnosisStatus {
	status := inventoryv1alpha1.OpenNebulaNodeHotplugDiagnosisStatus{
		Classification:    diagnosis.Classification,
		Operation:         diagnosis.Operation,
		VolumeHandle:      diagnosis.VolumeHandle,
		PersistentDiskID:  diagnosis.PersistentDiskID,
		VMDiskID:          diagnosis.VMDiskID,
		DiskTarget:        diagnosis.DiskTarget,
		DiskAttachFlag:    diagnosis.DiskAttachFlag,
		AgeSeconds:        diagnosis.AgeSeconds,
		StuckAfterSeconds: diagnosis.StuckAfterSeconds,
		Reason:            diagnosis.Reason,
		Message:           diagnosis.Message,
		RecommendedAction: diagnosis.RecommendedAction,
	}
	if !diagnosis.FirstObservedAt.IsZero() {
		value := metav1.NewTime(diagnosis.FirstObservedAt)
		status.FirstObservedAt = &value
	}
	if !diagnosis.LastObservedAt.IsZero() {
		value := metav1.NewTime(diagnosis.LastObservedAt)
		status.LastObservedAt = &value
	}
	if !diagnosis.LastChangedAt.IsZero() {
		value := metav1.NewTime(diagnosis.LastChangedAt)
		status.LastChangedAt = &value
	}
	return status
}

func loadInventoryHotplugDiagnosisConfig(cfg config.CSIPluginConfig) opennebula.HotplugDiagnosisConfig {
	stuckAfter := loadResync(cfg, config.HotplugDiagnosticsStuckAfterSecondsVar, 300)
	progressWindow := loadResync(cfg, config.HotplugDiagnosticsProgressWindowSecondsVar, 60)
	return opennebula.HotplugDiagnosisConfig{
		StuckAfter:     time.Duration(stuckAfter) * time.Second,
		ProgressWindow: time.Duration(progressWindow) * time.Second,
	}
}

func loadInventoryLeaderElectionID(cfg config.CSIPluginConfig) string {
	value, ok := cfg.GetString(config.InventoryControllerLeaderElectionIDVar)
	if !ok || strings.TrimSpace(value) == "" {
		return "opennebula-csi-inventory-controller"
	}
	return strings.TrimSpace(value)
}

func loadResync(cfg config.CSIPluginConfig, key string, fallback int) int {
	value, ok := cfg.GetInt(key)
	if !ok || value <= 0 {
		return fallback
	}
	return value
}
