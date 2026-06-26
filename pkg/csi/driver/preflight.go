package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/OpenNebula/one/src/oca/go/src/goca"
	datastoreSchema "github.com/OpenNebula/one/src/oca/go/src/goca/schemas/datastore"
	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/config"
	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/opennebula"
	utilexec "k8s.io/utils/exec"

	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type SecretRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type PreflightOptions struct {
	Datastores               []string
	NodeStageSecretRefs      []SecretRef
	ProvisionerSecretRefs    []SecretRef
	RequireSnapshotCRDs      bool
	RequireServiceMonitorCRD bool
}

type PreflightCheckResult struct {
	Check   string `json:"check"`
	Outcome string `json:"outcome"`
	Message string `json:"message"`
}

type PreflightStorageClassSummary struct {
	Name                 string   `json:"name"`
	Backend              string   `json:"backend"`
	Mounter              string   `json:"mounter,omitempty"`
	VolumeBindingMode    string   `json:"volumeBindingMode"`
	AllowVolumeExpansion bool     `json:"allowVolumeExpansion"`
	ValidationSupport    string   `json:"validationSupport"`
	Warnings             []string `json:"warnings,omitempty"`
}

type PreflightReport struct {
	Passed         bool                           `json:"passed"`
	Results        []PreflightCheckResult         `json:"results"`
	StorageClasses []PreflightStorageClassSummary `json:"storageClasses,omitempty"`
}

type PreflightRunner struct {
	Config  config.CSIPluginConfig
	Exec    utilexec.Interface
	Options PreflightOptions
}

func NewPreflightRunner(cfg config.CSIPluginConfig, exec utilexec.Interface, opts PreflightOptions) *PreflightRunner {
	if exec == nil {
		exec = utilexec.New()
	}

	return &PreflightRunner{
		Config:  cfg,
		Exec:    exec,
		Options: opts,
	}
}

func (r *PreflightRunner) Run(ctx context.Context) PreflightReport {
	results := make([]PreflightCheckResult, 0, 8)
	failed := false

	appendResult := func(check, outcome, message string) {
		results = append(results, PreflightCheckResult{
			Check:   check,
			Outcome: outcome,
			Message: message,
		})
		if outcome == "fail" {
			failed = true
		}
	}

	endpoint, _ := r.Config.GetString(config.OpenNebulaRPCEndpointVar)
	credentials, _ := r.Config.GetString(config.OpenNebulaCredentialsVar)
	if strings.TrimSpace(endpoint) == "" || strings.TrimSpace(credentials) == "" {
		appendResult("opennebula_auth", "fail", "ONE_XMLRPC and ONE_AUTH must both be configured")
		return PreflightReport{Passed: false, Results: results}
	}

	client := opennebula.NewClient(opennebula.OpenNebulaConfig{
		Endpoint:    endpoint,
		Credentials: credentials,
	})
	if err := client.Probe(ctx); err != nil {
		appendResult("opennebula_auth", "fail", fmt.Sprintf("failed to reach OpenNebula API: %v", err))
		return PreflightReport{Passed: false, Results: results}
	}
	appendResult("opennebula_auth", "pass", fmt.Sprintf("connected to OpenNebula API at %s", endpoint))

	ctrl := goca.NewController(client.Client)
	datastorePool, err := ctrl.Datastores().InfoContext(ctx)
	if err != nil {
		appendResult("datastore_resolution", "fail", fmt.Sprintf("failed to list OpenNebula datastores: %v", err))
		return PreflightReport{Passed: false, Results: results}
	}

	selection, hasDatastores, err := r.selectionConfig()
	if err != nil {
		appendResult("datastore_resolution", "fail", err.Error())
		return PreflightReport{Passed: false, Results: results}
	}

	resolvedDatastores := []opennebula.Datastore{}
	if !hasDatastores {
		appendResult("datastore_resolution", "skip", "no preflight datastores were configured; skipping datastore validation")
	} else {
		resolvedDatastores, err = opennebula.ResolveDatastores(datastorePool.Datastores, selection)
		if err != nil {
			appendResult("datastore_resolution", "fail", err.Error())
			return PreflightReport{Passed: false, Results: results}
		}
		names := make([]string, 0, len(resolvedDatastores))
		for _, ds := range resolvedDatastores {
			names = append(names, fmt.Sprintf("%d(%s:%s)", ds.ID, ds.Category, ds.Backend))
		}
		appendResult("datastore_resolution", "pass", fmt.Sprintf("validated datastores: %s", strings.Join(names, ", ")))
	}

	if err := r.requireBinary("ceph"); err != nil {
		appendResult("ceph_binary", "fail", err.Error())
	} else {
		appendResult("ceph_binary", "pass", "found ceph in PATH")
	}

	if err := r.requireBinary("ceph-fuse"); err != nil {
		appendResult("ceph_fuse_binary", "fail", err.Error())
	} else {
		appendResult("ceph_fuse_binary", "pass", "found ceph-fuse in PATH")
	}

	monitorErr := r.checkMonitorReachability(ctx, resolvedDatastores)
	switch {
	case monitorErr == nil && len(resolvedDatastores) > 0:
		appendResult("ceph_monitor_reachability", "pass", "successfully reached all discovered Ceph/CephFS monitors")
	case monitorErr == nil:
		appendResult("ceph_monitor_reachability", "skip", "no Ceph or CephFS datastores selected; skipping monitor reachability checks")
	default:
		appendResult("ceph_monitor_reachability", "fail", monitorErr.Error())
	}

	kubeClient, discoClient, kubeErr := preflightKubeClients()
	if kubeErr != nil {
		if len(r.Options.NodeStageSecretRefs) > 0 || len(r.Options.ProvisionerSecretRefs) > 0 || r.Options.RequireSnapshotCRDs || r.Options.RequireServiceMonitorCRD {
			appendResult("kubernetes_api", "fail", fmt.Sprintf("failed to initialize Kubernetes client for preflight: %v", kubeErr))
		} else {
			appendResult("kubernetes_api", "skip", "Kubernetes client not available; skipping cluster-scoped preflight checks")
		}
		appendResult("cephfs_kernel_mount_prereqs", "skip", "Kubernetes client not available; skipping CephFS kernel StorageClass audit")
	} else {
		appendResult("kubernetes_api", "pass", "connected to Kubernetes API for cluster-scoped preflight checks")

		if err := checkSecretRefs(ctx, kubeClient, append(r.Options.NodeStageSecretRefs, r.Options.ProvisionerSecretRefs...)...); err != nil {
			appendResult("secret_refs", "fail", err.Error())
		} else if len(r.Options.NodeStageSecretRefs)+len(r.Options.ProvisionerSecretRefs) > 0 {
			appendResult("secret_refs", "pass", "validated referenced Kubernetes secrets")
		} else {
			appendResult("secret_refs", "skip", "no secret references were configured for preflight validation")
		}

		if r.Options.RequireSnapshotCRDs {
			if err := requireAPIResource(discoClient, "snapshot.storage.k8s.io/v1", "volumesnapshots", "volumesnapshotcontents"); err != nil {
				appendResult("snapshot_crds", "fail", err.Error())
			} else {
				appendResult("snapshot_crds", "pass", "snapshot.storage.k8s.io CRDs are available")
			}
		} else {
			appendResult("snapshot_crds", "skip", "snapshot CRD validation was not requested")
		}

		if r.Options.RequireServiceMonitorCRD {
			if err := requireAPIResource(discoClient, "monitoring.coreos.com/v1", "servicemonitors"); err != nil {
				appendResult("servicemonitor_crd", "fail", err.Error())
			} else {
				appendResult("servicemonitor_crd", "pass", "monitoring.coreos.com ServiceMonitor CRD is available")
			}
		} else {
			appendResult("servicemonitor_crd", "skip", "ServiceMonitor CRD validation was not requested")
		}

		storageClassSummaries := r.inspectStorageClasses(ctx, kubeClient, datastorePool.Datastores)
		if anyCephFSKernelMounter(storageClassSummaries) {
			switch err := r.validateCephFSKernelMounterRuntime(); {
			case err == nil:
				appendResult("cephfs_kernel_mount_prereqs", "pass", "kernel CephFS StorageClasses have local runtime prerequisites available")
			default:
				appendResult("cephfs_kernel_mount_prereqs", "fail", err.Error())
			}
		} else {
			appendResult("cephfs_kernel_mount_prereqs", "skip", "no CephFS StorageClasses request kernel mounts")
		}

		outcome, message := r.checkLocalImmediateBinding(ctx, kubeClient, datastorePool.Datastores)
		appendResult("local_immediate_binding", outcome, message)
		results = append(results, PreflightCheckResult{
			Check:   "storageclass_audit",
			Outcome: "pass",
			Message: fmt.Sprintf("inspected %d CSI StorageClasses", len(storageClassSummaries)),
		})
		return PreflightReport{
			Passed:         !failed,
			Results:        results,
			StorageClasses: storageClassSummaries,
		}
	}

	return PreflightReport{
		Passed:         !failed,
		Results:        results,
		StorageClasses: nil,
	}
}

func RunPreflightCommand(ctx context.Context, cfg config.CSIPluginConfig, exec utilexec.Interface, opts PreflightOptions, output string, w io.Writer) error {
	report := NewPreflightRunner(cfg, exec, opts).Run(ctx)

	switch strings.ToLower(strings.TrimSpace(output)) {
	case "", "text":
		for _, result := range report.Results {
			fmt.Fprintf(w, "[%s] %s: %s\n", strings.ToUpper(result.Outcome), result.Check, result.Message)
		}
		if len(report.StorageClasses) > 0 {
			fmt.Fprintln(w, "StorageClasses:")
			for _, summary := range report.StorageClasses {
				warnings := "-"
				if len(summary.Warnings) > 0 {
					warnings = strings.Join(summary.Warnings, "; ")
				}
				fmt.Fprintf(w, "- %s backend=%s mounter=%s binding=%s expansion=%t validation=%s warnings=%s\n",
					summary.Name, summary.Backend, summary.Mounter, summary.VolumeBindingMode, summary.AllowVolumeExpansion, summary.ValidationSupport, warnings)
			}
		}
	case "json":
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			return fmt.Errorf("failed to encode preflight report: %w", err)
		}
	default:
		return fmt.Errorf("unsupported preflight output format %q", output)
	}

	if !report.Passed {
		return fmt.Errorf("preflight checks failed")
	}

	return nil
}

func ParseSecretRefsCSV(raw string) ([]SecretRef, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	refs := make([]SecretRef, 0)
	for _, part := range strings.Split(raw, ",") {
		value := strings.TrimSpace(part)
		if value == "" {
			continue
		}

		namespace, name, ok := strings.Cut(value, "/")
		if !ok || strings.TrimSpace(namespace) == "" || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("invalid secret reference %q; expected namespace/name", value)
		}

		refs = append(refs, SecretRef{
			Namespace: strings.TrimSpace(namespace),
			Name:      strings.TrimSpace(name),
		})
	}

	return refs, nil
}

func (r *PreflightRunner) selectionConfig() (opennebula.DatastoreSelectionConfig, bool, error) {
	cfg := r.Config
	if len(r.Options.Datastores) > 0 {
		cfg.OverrideVal(config.DefaultDatastoresVar, strings.Join(r.Options.Datastores, ","))
	}

	driver := &Driver{PluginConfig: cfg}
	selection, err := driver.GetDatastoreSelectionConfig(nil)
	if err != nil {
		if len(r.Options.Datastores) == 0 {
			if defaults, ok := cfg.GetStringSlice(config.DefaultDatastoresVar); !ok || len(defaults) == 0 {
				return opennebula.DatastoreSelectionConfig{}, false, nil
			}
		}
		return opennebula.DatastoreSelectionConfig{}, false, err
	}

	return selection, true, nil
}

func (r *PreflightRunner) requireBinary(name string) error {
	if r.Exec == nil {
		return fmt.Errorf("exec interface is not configured")
	}
	if _, err := r.Exec.LookPath(name); err != nil {
		return fmt.Errorf("required binary %q was not found in PATH", name)
	}
	return nil
}

func (r *PreflightRunner) checkMonitorReachability(ctx context.Context, datastores []opennebula.Datastore) error {
	uniqueTargets := make(map[string]struct{})
	for _, datastore := range datastores {
		switch datastore.Backend {
		case "ceph":
			if datastore.Ceph == nil {
				continue
			}
			for _, target := range expandMonitorTargets(datastore.Ceph.CephHost) {
				uniqueTargets[target] = struct{}{}
			}
		case "cephfs":
			if datastore.CephFS == nil {
				continue
			}
			for _, target := range expandMonitorTargets(strings.Join(datastore.CephFS.Monitors, ",")) {
				uniqueTargets[target] = struct{}{}
			}
		}
	}

	if len(uniqueTargets) == 0 {
		return nil
	}

	for target := range uniqueTargets {
		conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", target)
		if err != nil {
			return fmt.Errorf("failed to reach Ceph monitor %s: %w", target, err)
		}
		_ = conn.Close()
	}

	return nil
}

func expandMonitorTargets(raw string) []string {
	replacer := strings.NewReplacer(",", " ", ";", " ")
	parts := strings.Fields(replacer.Replace(strings.TrimSpace(raw)))
	targets := make([]string, 0, len(parts)*2)
	seen := make(map[string]struct{})

	appendTarget := func(value string) {
		if value == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		targets = append(targets, value)
	}

	for _, part := range parts {
		if _, _, err := net.SplitHostPort(part); err == nil {
			appendTarget(part)
			continue
		}

		host := strings.Trim(part, "[]")
		appendTarget(net.JoinHostPort(host, "3300"))
		appendTarget(net.JoinHostPort(host, "6789"))
	}

	return targets
}

func checkSecretRefs(ctx context.Context, client kubernetes.Interface, refs ...SecretRef) error {
	for _, ref := range refs {
		if strings.TrimSpace(ref.Namespace) == "" || strings.TrimSpace(ref.Name) == "" {
			return fmt.Errorf("secret reference must include namespace and name")
		}

		_, err := client.CoreV1().Secrets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err == nil {
			continue
		}
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("secret %s/%s was not found", ref.Namespace, ref.Name)
		}
		return fmt.Errorf("failed to read secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	return nil
}

func requireAPIResource(discoveryClient discovery.DiscoveryInterface, groupVersion string, resources ...string) error {
	resourceList, err := discoveryClient.ServerResourcesForGroupVersion(groupVersion)
	if err != nil {
		return fmt.Errorf("required API group %s is not available: %w", groupVersion, err)
	}

	available := make(map[string]struct{}, len(resourceList.APIResources))
	for _, resource := range resourceList.APIResources {
		available[resource.Name] = struct{}{}
	}

	for _, resource := range resources {
		if _, ok := available[resource]; !ok {
			return fmt.Errorf("required resource %s in API group %s is not available", resource, groupVersion)
		}
	}

	return nil
}

func preflightKubeClients() (kubernetes.Interface, discovery.DiscoveryInterface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
		if kubeconfig := strings.TrimSpace(os.Getenv("KUBECONFIG")); kubeconfig != "" {
			loadingRules.ExplicitPath = kubeconfig
		} else if home, homeErr := os.UserHomeDir(); homeErr == nil {
			loadingRules.ExplicitPath = filepath.Join(home, ".kube", "config")
		}

		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			loadingRules,
			&clientcmd.ConfigOverrides{},
		).ClientConfig()
		if err != nil {
			return nil, nil, err
		}
	}

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}

	return client, client.Discovery(), nil
}

func (r *PreflightRunner) checkLocalImmediateBinding(ctx context.Context, client kubernetes.Interface, pool []datastoreSchema.Datastore) (string, string) {
	policy := normalizeLocalImmediateBindingPolicy(r.Config)
	storageClasses, err := client.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "fail", fmt.Sprintf("failed to list storage classes for local binding validation: %v", err)
	}

	warnings := make([]string, 0)
	for _, storageClass := range storageClasses.Items {
		if storageClass.Provisioner != DefaultDriverName {
			continue
		}
		if !storageClassUsesLocalBackend(r.Config, pool, storageClass) {
			continue
		}
		if volumeBindingMode(storageClass) != storagev1.VolumeBindingWaitForFirstConsumer {
			warnings = append(warnings, fmt.Sprintf("%s uses Immediate binding for local-backed provisioning", storageClass.Name))
		}
	}

	if len(warnings) == 0 {
		return "pass", "no local-backed StorageClasses use Immediate binding"
	}
	if policy == "fail" {
		return "fail", strings.Join(warnings, "; ")
	}
	return "warn", strings.Join(warnings, "; ")
}

func (r *PreflightRunner) inspectStorageClasses(ctx context.Context, client kubernetes.Interface, pool []datastoreSchema.Datastore) []PreflightStorageClassSummary {
	storageClasses, err := client.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	summaries := make([]PreflightStorageClassSummary, 0)
	for _, storageClass := range storageClasses.Items {
		if storageClass.Provisioner != DefaultDriverName {
			continue
		}
		backend := storageClassBackendSummary(r.Config, pool, storageClass)
		mounter := ""
		if backend == "cephfs" {
			normalized, err := normalizeSharedFilesystemMounter(storageClass.Parameters[storageClassParamCephFSMounter])
			if err != nil {
				mounter = strings.TrimSpace(storageClass.Parameters[storageClassParamCephFSMounter])
			} else {
				mounter = string(normalized)
			}
		}
		mode := string(volumeBindingMode(storageClass))
		allowExpansion := false
		if storageClass.AllowVolumeExpansion != nil {
			allowExpansion = *storageClass.AllowVolumeExpansion
		}
		summary := PreflightStorageClassSummary{
			Name:                 storageClass.Name,
			Backend:              backend,
			Mounter:              mounter,
			VolumeBindingMode:    mode,
			AllowVolumeExpansion: allowExpansion,
			ValidationSupport:    validationSupportForBackend(backend),
		}
		if backend == "mixed" {
			summary.Warnings = append(summary.Warnings, "storage class mixes CephFS and disk datastores; use separate StorageClasses per backend")
		}
		if backend == "local" && volumeBindingMode(storageClass) != storagev1.VolumeBindingWaitForFirstConsumer {
			summary.Warnings = append(summary.Warnings, "local datastore should use WaitForFirstConsumer")
		}
		if !allowExpansion {
			summary.Warnings = append(summary.Warnings, "volume expansion disabled")
		}
		if backend == "cephfs" {
			if rawMounter := strings.TrimSpace(storageClass.Parameters[storageClassParamCephFSMounter]); rawMounter != "" {
				if _, err := normalizeSharedFilesystemMounter(rawMounter); err != nil {
					summary.Warnings = append(summary.Warnings, err.Error())
				}
			}
			if !hasCompleteCSISecretRef(storageClass.Parameters, "csi.storage.k8s.io/provisioner-secret-name", "csi.storage.k8s.io/provisioner-secret-namespace") {
				summary.Warnings = append(summary.Warnings, "cephfs provisioner secret refs incomplete")
			}
			if !hasCompleteCSISecretRef(storageClass.Parameters, "csi.storage.k8s.io/node-stage-secret-name", "csi.storage.k8s.io/node-stage-secret-namespace") {
				summary.Warnings = append(summary.Warnings, "cephfs node-stage secret refs incomplete")
			}
			if allowExpansion && !hasCompleteCSISecretRef(storageClass.Parameters, "csi.storage.k8s.io/controller-expand-secret-name", "csi.storage.k8s.io/controller-expand-secret-namespace") {
				summary.Warnings = append(summary.Warnings, "cephfs controller-expand secret refs incomplete")
			}
			if mounter == string(sharedFilesystemMounterKernel) {
				if !loadFeatureGates(r.Config).CephFSKernelMounts {
					summary.Warnings = append(summary.Warnings, "cephfs kernel mounts requested but feature gate cephfsKernelMounts is disabled")
				}
				if err := r.validateCephFSKernelMounterRuntime(); err != nil {
					summary.Warnings = append(summary.Warnings, err.Error())
				}
			}
		}
		summaries = append(summaries, summary)
	}
	return summaries
}

func anyCephFSKernelMounter(summaries []PreflightStorageClassSummary) bool {
	for _, summary := range summaries {
		if summary.Backend == "cephfs" && summary.Mounter == string(sharedFilesystemMounterKernel) {
			return true
		}
	}
	return false
}

func (r *PreflightRunner) validateCephFSKernelMounterRuntime() error {
	if err := r.requireBinary("mount.ceph"); err != nil {
		return err
	}
	if err := sharedFilesystemKernelSupported(); err != nil {
		return err
	}
	return nil
}

func validationSupportForBackend(_ string) string {
	return "manual"
}

func storageClassBackendSummary(cfg config.CSIPluginConfig, pool []datastoreSchema.Datastore, storageClass storagev1.StorageClass) string {
	driver := &Driver{PluginConfig: cfg}
	selection, err := driver.GetDatastoreSelectionConfig(storageClass.Parameters)
	if err == nil {
		datastores, resolveErr := opennebula.ResolveDatastores(pool, selection)
		if resolveErr == nil && len(datastores) > 0 {
			backend := datastores[0].Backend
			for _, datastore := range datastores[1:] {
				if datastore.Backend != backend {
					return "mixed"
				}
			}
			switch backend {
			case "ceph":
				return "ceph-rbd"
			default:
				return backend
			}
		}
	}
	if storageClassUsesLocalBackend(cfg, pool, storageClass) {
		return "local"
	}
	driverName := strings.ToLower(strings.TrimSpace(storageClass.Parameters["driver"]))
	switch driverName {
	case "cephfs":
		return "cephfs"
	case "ceph", "rbd":
		return "ceph-rbd"
	}
	if strings.Contains(strings.ToLower(storageClass.Provisioner), "cephfs") {
		return "cephfs"
	}
	return "unknown"
}

func hasCompleteCSISecretRef(params map[string]string, nameKey, namespaceKey string) bool {
	return strings.TrimSpace(params[nameKey]) != "" && strings.TrimSpace(params[namespaceKey]) != ""
}

func normalizeLocalImmediateBindingPolicy(cfg config.CSIPluginConfig) string {
	value, ok := cfg.GetString(config.PreflightLocalImmediateBindingPolicyVar)
	if !ok {
		return "warn"
	}
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "fail":
		return "fail"
	default:
		return "warn"
	}
}

func storageClassUsesLocalBackend(cfg config.CSIPluginConfig, pool []datastoreSchema.Datastore, storageClass storagev1.StorageClass) bool {
	driver := &Driver{PluginConfig: cfg}
	selection, err := driver.GetDatastoreSelectionConfig(storageClass.Parameters)
	if err != nil {
		return false
	}

	datastores, err := opennebula.ResolveDatastores(pool, selection)
	if err != nil {
		return false
	}

	for _, datastore := range datastores {
		if datastore.Backend == "local" {
			return true
		}
	}

	return false
}

func volumeBindingMode(storageClass storagev1.StorageClass) storagev1.VolumeBindingMode {
	if storageClass.VolumeBindingMode == nil {
		return storagev1.VolumeBindingImmediate
	}
	return *storageClass.VolumeBindingMode
}
