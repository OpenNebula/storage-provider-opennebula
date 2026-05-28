package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/config"
	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/opennebula"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const lastNodePreferenceWebhookPath = "/mutate-v1-pod-last-node-preference"

type jsonPatchOperation struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

type LastNodePreferenceWebhook struct {
	driver *Driver
}

func NewLastNodePreferenceWebhook(driver *Driver) *LastNodePreferenceWebhook {
	return &LastNodePreferenceWebhook{driver: driver}
}

func (w *LastNodePreferenceWebhook) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		rw.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var review admissionv1.AdmissionReview
	if err := json.NewDecoder(req.Body).Decode(&review); err != nil {
		rw.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(rw).Encode(admissionv1.AdmissionReview{
			Response: &admissionv1.AdmissionResponse{
				UID:     review.Request.UID,
				Allowed: false,
				Result:  &metav1.Status{Message: err.Error()},
			},
		})
		return
	}

	response := &admissionv1.AdmissionResponse{
		UID:     review.Request.UID,
		Allowed: true,
	}
	if review.Request == nil || review.Request.Kind.Kind != "Pod" {
		review.Response = response
		_ = json.NewEncoder(rw).Encode(review)
		return
	}

	var pod corev1.Pod
	if err := json.Unmarshal(review.Request.Object.Raw, &pod); err != nil {
		response.Allowed = false
		response.Result = &metav1.Status{Message: err.Error()}
		review.Response = response
		_ = json.NewEncoder(rw).Encode(review)
		return
	}

	patch, warnings, err := w.buildPatch(req.Context(), &pod)
	if err != nil {
		response.Allowed = false
		response.Result = &metav1.Status{Message: err.Error()}
		review.Response = response
		_ = json.NewEncoder(rw).Encode(review)
		return
	}
	if len(warnings) > 0 {
		response.Warnings = warnings
	}
	if len(patch) > 0 {
		rawPatch, err := json.Marshal(patch)
		if err != nil {
			response.Allowed = false
			response.Result = &metav1.Status{Message: err.Error()}
			review.Response = response
			_ = json.NewEncoder(rw).Encode(review)
			return
		}
		patchType := admissionv1.PatchTypeJSONPatch
		response.PatchType = &patchType
		response.Patch = rawPatch
	}
	review.Response = response
	_ = json.NewEncoder(rw).Encode(review)
}

func (w *LastNodePreferenceWebhook) buildPatch(ctx context.Context, pod *corev1.Pod) ([]jsonPatchOperation, []string, error) {
	if w == nil || w.driver == nil || w.driver.kubeRuntime == nil || !w.driver.kubeRuntime.enabled {
		return nil, nil, nil
	}
	if pod == nil || strings.TrimSpace(pod.Spec.NodeName) != "" {
		return nil, nil, nil
	}
	if !w.lastNodePreferenceEnabled() {
		w.driver.metrics.RecordLastNodePreference("skipped", "feature_disabled")
		return nil, nil, nil
	}
	if strings.EqualFold(strings.TrimSpace(pod.Annotations[annotationLastNodePref]), lastNodePreferenceDisabledValue) {
		w.driver.metrics.RecordLastNodePreference("skipped", "pod_opt_out")
		return nil, nil, nil
	}

	maintenanceActive := w.driver.maintenanceMode != nil && w.driver.maintenanceMode.Active()
	targetNode, warnings, reason, err := w.resolvePreferredNode(ctx, pod)
	if err != nil {
		w.driver.metrics.RecordLastNodePreference("error", "resolution_failed")
		return nil, warnings, err
	}
	if maintenanceActive && reason == "conflicting_nodes" {
		w.driver.metrics.RecordLastNodePreference("error", "maintenance_conflicting_nodes")
		return nil, warnings, fmt.Errorf("maintenance mode requires a single last attached node for local RWO PVCs")
	}
	if strings.TrimSpace(targetNode) == "" {
		w.driver.metrics.RecordLastNodePreference("skipped", reason)
		return nil, warnings, nil
	}
	hostname, err := w.driver.kubeRuntime.GetNodeHostname(ctx, targetNode)
	if err != nil {
		w.driver.metrics.RecordLastNodePreference("error", "node_lookup_failed")
		return nil, warnings, err
	}
	if !maintenanceActive && hasPreferredHostnameAffinity(pod, hostname) {
		w.driver.metrics.RecordLastNodePreference("skipped", "already_present")
		return nil, warnings, nil
	}

	var patch []jsonPatchOperation
	if maintenanceActive {
		patch = buildLastNodeRequiredPatch(pod, targetNode, hostname)
		w.driver.metrics.RecordLastNodePreference("injected", "maintenance_required_hostname")
		return patch, warnings, nil
	}
	patch = buildLastNodePreferencePatch(pod, targetNode, hostname)
	w.driver.metrics.RecordLastNodePreference("injected", "preferred_hostname")
	return patch, warnings, nil
}

func (w *LastNodePreferenceWebhook) resolvePreferredNode(ctx context.Context, pod *corev1.Pod) (string, []string, string, error) {
	nodes := map[string]struct{}{}
	warnings := []string{}
	for _, volume := range pod.Spec.Volumes {
		if volume.PersistentVolumeClaim == nil {
			continue
		}
		pvc, err := w.driver.kubeRuntime.client.CoreV1().PersistentVolumeClaims(pod.Namespace).Get(ctx, volume.PersistentVolumeClaim.ClaimName, metav1.GetOptions{})
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("skipping PVC %s: %v", volume.PersistentVolumeClaim.ClaimName, err))
			continue
		}
		if strings.EqualFold(strings.TrimSpace(pvc.Annotations[annotationLastNodePref]), lastNodePreferenceDisabledValue) {
			continue
		}
		if strings.TrimSpace(pvc.Spec.VolumeName) == "" {
			continue
		}
		pv, err := w.driver.kubeRuntime.client.CoreV1().PersistentVolumes().Get(ctx, pvc.Spec.VolumeName, metav1.GetOptions{})
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("skipping PV %s: %v", pvc.Spec.VolumeName, err))
			continue
		}
		if !eligibleForLastNodePreference(pv) {
			continue
		}
		lastNode := strings.TrimSpace(pv.Annotations[annotationLastAttachedNode])
		if lastNode == "" {
			continue
		}
		nodes[lastNode] = struct{}{}
	}
	switch len(nodes) {
	case 0:
		return "", warnings, "no_eligible_volume", nil
	case 1:
		for node := range nodes {
			return node, warnings, "eligible", nil
		}
	}
	warnings = append(warnings, "skipping soft last-node preference because referenced PVCs disagree on preferred node")
	return "", warnings, "conflicting_nodes", nil
}

func (w *LastNodePreferenceWebhook) lastNodePreferenceEnabled() bool {
	if w == nil || w.driver == nil {
		return false
	}
	enabled, ok := w.driver.PluginConfig.GetBool(config.LastNodePreferenceEnabledVar)
	if !ok {
		return true
	}
	return enabled
}

func eligibleForLastNodePreference(pv *corev1.PersistentVolume) bool {
	if pv == nil || pv.Spec.CSI == nil {
		return false
	}
	if strings.TrimSpace(pv.Spec.CSI.Driver) != DefaultDriverName {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(pv.Annotations[annotationBackend]), "local") {
		return false
	}
	if !hasSingleWriterAccessMode(pv.Spec.AccessModes) {
		return false
	}
	if opennebula.IsSharedFilesystemVolumeID(pv.Spec.CSI.VolumeHandle) {
		return false
	}
	return true
}

func hasPreferredHostnameAffinity(pod *corev1.Pod, hostname string) bool {
	if pod == nil || pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil {
		return false
	}
	for _, term := range pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
		for _, expr := range term.Preference.MatchExpressions {
			if expr.Key != corev1.LabelHostname || expr.Operator != corev1.NodeSelectorOpIn {
				continue
			}
			for _, value := range expr.Values {
				if value == hostname {
					return true
				}
			}
		}
	}
	return false
}

func buildLastNodePreferencePatch(pod *corev1.Pod, targetNode, hostname string) []jsonPatchOperation {
	patch := make([]jsonPatchOperation, 0, 8)
	if pod.Annotations == nil {
		patch = append(patch, jsonPatchOperation{Op: "add", Path: "/metadata/annotations", Value: map[string]string{}})
	}
	patch = append(patch,
		jsonPatchOperation{Op: "add", Path: "/metadata/annotations/" + jsonPointerEscape(annotationPreferredLastNode), Value: targetNode},
		jsonPatchOperation{Op: "add", Path: "/metadata/annotations/" + jsonPointerEscape(annotationLastNodeInjected), Value: "true"},
	)
	if pod.Spec.Affinity == nil {
		patch = append(patch, jsonPatchOperation{Op: "add", Path: "/spec/affinity", Value: corev1.Affinity{}})
	}
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil {
		patch = append(patch, jsonPatchOperation{Op: "add", Path: "/spec/affinity/nodeAffinity", Value: corev1.NodeAffinity{}})
	}
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil || pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution == nil {
		patch = append(patch, jsonPatchOperation{Op: "add", Path: "/spec/affinity/nodeAffinity/preferredDuringSchedulingIgnoredDuringExecution", Value: []corev1.PreferredSchedulingTerm{}})
	}
	patch = append(patch, jsonPatchOperation{
		Op:   "add",
		Path: "/spec/affinity/nodeAffinity/preferredDuringSchedulingIgnoredDuringExecution/-",
		Value: corev1.PreferredSchedulingTerm{
			Weight: 100,
			Preference: corev1.NodeSelectorTerm{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key:      corev1.LabelHostname,
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{hostname},
				}},
			},
		},
	})
	return patch
}

func buildLastNodeRequiredPatch(pod *corev1.Pod, targetNode, hostname string) []jsonPatchOperation {
	patch := make([]jsonPatchOperation, 0, 4)
	if pod.Annotations == nil {
		patch = append(patch, jsonPatchOperation{Op: "add", Path: "/metadata/annotations", Value: map[string]string{}})
	}
	patch = append(patch,
		jsonPatchOperation{Op: "add", Path: "/metadata/annotations/" + jsonPointerEscape(annotationPreferredLastNode), Value: targetNode},
		jsonPatchOperation{Op: "add", Path: "/metadata/annotations/" + jsonPointerEscape(annotationLastNodeInjected), Value: "true"},
	)

	affinity := corev1.Affinity{}
	affinityOp := "add"
	if pod.Spec.Affinity != nil {
		affinity = *pod.Spec.Affinity.DeepCopy()
		affinityOp = "replace"
	}
	if affinity.NodeAffinity == nil {
		affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	requirement := corev1.NodeSelectorRequirement{
		Key:      corev1.LabelHostname,
		Operator: corev1.NodeSelectorOpIn,
		Values:   []string{hostname},
	}
	required := affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if required == nil || len(required.NodeSelectorTerms) == 0 {
		affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{requirement},
			}},
		}
	} else {
		for i := range required.NodeSelectorTerms {
			if !nodeSelectorTermHasHostname(required.NodeSelectorTerms[i], hostname) {
				required.NodeSelectorTerms[i].MatchExpressions = append(required.NodeSelectorTerms[i].MatchExpressions, requirement)
			}
		}
		affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = required
	}
	patch = append(patch, jsonPatchOperation{
		Op:    affinityOp,
		Path:  "/spec/affinity",
		Value: affinity,
	})
	return patch
}

func nodeSelectorTermHasHostname(term corev1.NodeSelectorTerm, hostname string) bool {
	for _, expr := range term.MatchExpressions {
		if expr.Key != corev1.LabelHostname || expr.Operator != corev1.NodeSelectorOpIn {
			continue
		}
		for _, value := range expr.Values {
			if value == hostname {
				return true
			}
		}
	}
	return false
}

func jsonPointerEscape(value string) string {
	value = strings.ReplaceAll(value, "~", "~0")
	value = strings.ReplaceAll(value, "/", "~1")
	return value
}
