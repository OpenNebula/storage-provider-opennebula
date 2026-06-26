package driver

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/config"
	"github.com/SparkAIUR/storage-provider-opennebula/pkg/csi/opennebula"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

type AttachmentReconciler struct {
	server       *ControllerServer
	interval     time.Duration
	orphanGrace  time.Duration
	staleVAGrace time.Duration

	mu              sync.Mutex
	orphanSeen      map[string]time.Time
	staleVASeen     map[string]time.Time
	divergentSeen   map[string]time.Time
	multiAttachSeen map[string]time.Time
}

func NewAttachmentReconciler(server *ControllerServer) *AttachmentReconciler {
	intervalSeconds, ok := server.driver.PluginConfig.GetInt(config.StuckAttachmentReconcilerIntervalVar)
	if !ok || intervalSeconds <= 0 {
		intervalSeconds = 60
	}
	orphanGraceSeconds, ok := server.driver.PluginConfig.GetInt(config.StuckAttachmentOrphanGraceSecondsVar)
	if !ok || orphanGraceSeconds <= 0 {
		orphanGraceSeconds = 120
	}
	staleVAGraceSeconds, ok := server.driver.PluginConfig.GetInt(config.StuckAttachmentStaleVAGraceSecondsVar)
	if !ok || staleVAGraceSeconds <= 0 {
		staleVAGraceSeconds = 90
	}
	return &AttachmentReconciler{
		server:          server,
		interval:        time.Duration(intervalSeconds) * time.Second,
		orphanGrace:     time.Duration(orphanGraceSeconds) * time.Second,
		staleVAGrace:    time.Duration(staleVAGraceSeconds) * time.Second,
		orphanSeen:      map[string]time.Time{},
		staleVASeen:     map[string]time.Time{},
		divergentSeen:   map[string]time.Time{},
		multiAttachSeen: map[string]time.Time{},
	}
}

func (r *AttachmentReconciler) Run(ctx context.Context) {
	if r == nil || r.server == nil || r.server.driver == nil || r.server.driver.kubeRuntime == nil || !r.server.driver.kubeRuntime.enabled {
		return
	}
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if r.server.driver.controllerLeadership != nil && !r.server.driver.controllerLeadership.IsLeader() {
				continue
			}
			if err := r.ReconcileOnce(ctx); err != nil {
				klog.V(2).InfoS("Attachment reconciler loop failed", "err", err)
			}
		}
	}
}

func (r *AttachmentReconciler) ReconcileOnce(ctx context.Context) error {
	client := r.server.driver.kubeRuntime.client
	if client == nil {
		return nil
	}

	attachments, err := r.server.volumeProvider.ListCurrentAttachments(ctx)
	if err != nil {
		return err
	}
	vaList, err := client.StorageV1().VolumeAttachments().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	pvList, err := client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	podList, err := client.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}

	pvByName := make(map[string]*corev1.PersistentVolume, len(pvList.Items))
	pvByHandle := make(map[string]*corev1.PersistentVolume, len(pvList.Items))
	for idx := range pvList.Items {
		pv := &pvList.Items[idx]
		pvByName[pv.Name] = pv
		if pv.Spec.CSI != nil {
			pvByHandle[pv.Spec.CSI.VolumeHandle] = pv
		}
	}

	activePVCUsers := make(map[string]bool)
	for idx := range podList.Items {
		pod := &podList.Items[idx]
		if pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, volume := range pod.Spec.Volumes {
			if volume.PersistentVolumeClaim == nil {
				continue
			}
			activePVCUsers[pod.Namespace+"/"+volume.PersistentVolumeClaim.ClaimName] = true
		}
	}

	attachmentsByVolume := make(map[string][]opennebula.ObservedAttachment)
	attachmentByVolumeNode := make(map[string]opennebula.ObservedAttachment)
	for _, attachment := range attachments {
		attachmentsByVolume[attachment.VolumeHandle] = append(attachmentsByVolume[attachment.VolumeHandle], attachment)
		attachmentByVolumeNode[attachment.VolumeHandle+"@"+attachment.NodeName] = attachment
	}

	vaByVolume := make(map[string][]storagev1.VolumeAttachment)
	for _, va := range vaList.Items {
		if va.Spec.Source.PersistentVolumeName == nil {
			continue
		}
		pv := pvByName[*va.Spec.Source.PersistentVolumeName]
		if pv == nil || pv.Spec.CSI == nil {
			continue
		}
		vaByVolume[pv.Spec.CSI.VolumeHandle] = append(vaByVolume[pv.Spec.CSI.VolumeHandle], va)
	}

	now := time.Now().UTC()
	currentOrphans := map[string]struct{}{}
	currentStaleVAs := map[string]struct{}{}
	currentDivergent := map[string]struct{}{}
	currentMultiAttach := map[string]struct{}{}

	for volumeHandle, observed := range attachmentsByVolume {
		pv := pvByHandle[volumeHandle]
		if pv == nil || pv.Spec.CSI == nil {
			continue
		}
		if r.skipVolume(volumeHandle) {
			continue
		}
		pvcKey := pvcKeyForPV(pv)
		activePodUsingPVC := pvcKey != "" && activePVCUsers[pvcKey]
		vas := vaByVolume[volumeHandle]
		singleWriter := hasSingleWriterAccessMode(pv.Spec.AccessModes)
		if singleWriter && len(observed) > 1 {
			currentMultiAttach[volumeHandle] = struct{}{}
			r.server.driver.metrics.RecordAttachmentReconciler("multi_attach", "warn", "observed")
			r.recordPVCEventForVolume(ctx, volumeHandle, eventReasonStuckAttachmentDetached, fmt.Sprintf("detected multiple attachments for single-writer volume %s", volumeHandle))
		}

		for _, attachment := range observed {
			key := volumeHandle + "@" + attachment.NodeName
			var matchingVA *storagev1.VolumeAttachment
			for idx := range vas {
				if strings.TrimSpace(vas[idx].Spec.NodeName) == strings.TrimSpace(attachment.NodeName) && vas[idx].Status.Attached {
					matchingVA = &vas[idx]
					break
				}
			}
			if matchingVA != nil {
				continue
			}
			if activePodUsingPVC {
				continue
			}
			if len(vas) > 0 {
				currentDivergent[key] = struct{}{}
				if !r.due(r.divergentSeen, key, now, r.orphanGrace) {
					continue
				}
				if err := r.detachObservedAttachment(ctx, attachment, hotplugQueuePriorityCritical, "divergent_attachment"); err != nil {
					r.server.driver.metrics.RecordAttachmentReconciler("divergent_attachment", "detach", "error")
					klog.V(2).InfoS("Failed to detach divergent attachment", "volume", volumeHandle, "node", attachment.NodeName, "err", err)
				} else {
					r.server.driver.metrics.RecordAttachmentReconciler("divergent_attachment", "detach", "success")
					r.recordPVCEventForVolume(ctx, volumeHandle, eventReasonStuckAttachmentDetached, fmt.Sprintf("detached divergent attachment for volume %s from node %s", volumeHandle, attachment.NodeName))
				}
				continue
			}

			currentOrphans[key] = struct{}{}
			if !r.due(r.orphanSeen, key, now, r.orphanGrace) {
				continue
			}
			if err := r.detachObservedAttachment(ctx, attachment, hotplugQueuePriorityBackground, "orphan_attachment"); err != nil {
				r.server.driver.metrics.RecordAttachmentReconciler("orphan_attachment", "detach", "error")
				klog.V(2).InfoS("Failed to detach orphan attachment", "volume", volumeHandle, "node", attachment.NodeName, "err", err)
			} else {
				r.server.driver.metrics.RecordAttachmentReconciler("orphan_attachment", "detach", "success")
				r.recordPVCEventForVolume(ctx, volumeHandle, eventReasonStuckAttachmentDetached, fmt.Sprintf("detached orphan attachment for volume %s from node %s", volumeHandle, attachment.NodeName))
			}
		}
	}

	for idx := range vaList.Items {
		va := &vaList.Items[idx]
		if va.Spec.Source.PersistentVolumeName == nil || !va.Status.Attached {
			continue
		}
		pv := pvByName[*va.Spec.Source.PersistentVolumeName]
		if pv == nil || pv.Spec.CSI == nil {
			continue
		}
		volumeHandle := pv.Spec.CSI.VolumeHandle
		if r.skipVolume(volumeHandle) {
			continue
		}
		if _, ok := attachmentByVolumeNode[volumeHandle+"@"+va.Spec.NodeName]; ok {
			continue
		}
		currentStaleVAs[va.Name] = struct{}{}
		if !r.due(r.staleVASeen, va.Name, now, r.staleVAGrace) {
			continue
		}
		if err := client.StorageV1().VolumeAttachments().Delete(ctx, va.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			r.server.driver.metrics.RecordAttachmentReconciler("stale_volume_attachment", "delete", "error")
			klog.V(2).InfoS("Failed to delete stale VolumeAttachment", "name", va.Name, "err", err)
			continue
		}
		r.server.driver.metrics.RecordAttachmentReconciler("stale_volume_attachment", "delete", "success")
		r.recordPVCEventForVolume(ctx, volumeHandle, eventReasonStuckAttachmentRepublish, fmt.Sprintf("deleted stale VolumeAttachment %s for volume %s to trigger republish", va.Name, volumeHandle))
	}

	r.prune(r.orphanSeen, currentOrphans)
	r.prune(r.staleVASeen, currentStaleVAs)
	r.prune(r.divergentSeen, currentDivergent)
	r.prune(r.multiAttachSeen, currentMultiAttach)
	return nil
}

func (r *AttachmentReconciler) skipVolume(volumeHandle string) bool {
	if r == nil || r.server == nil || r.server.driver == nil {
		return true
	}
	if r.server.driver.stickyAttachments != nil {
		if _, ok := r.server.driver.stickyAttachments.Get(volumeHandle); ok {
			return true
		}
	}
	if r.server.driver.hotplugQueue != nil && r.server.driver.hotplugQueue.HasVolume(volumeHandle) {
		return true
	}
	return false
}

func (r *AttachmentReconciler) detachObservedAttachment(ctx context.Context, attachment opennebula.ObservedAttachment, priority HotplugQueuePriority, reason string) error {
	return r.server.withQueuedHotplug(ctx, attachment.NodeName, "detach", attachment.VolumeHandle, priority, func(queueCtx context.Context) error {
		nodeRelease := r.server.driver.operationLocks.Acquire(controllerNodeLockKey(attachment.NodeName))
		defer nodeRelease()

		volumeRelease := r.server.driver.operationLocks.Acquire(controllerVolumeLockKey(attachment.VolumeHandle))
		defer volumeRelease()

		if r.server.driver.stickyAttachments != nil {
			if _, ok := r.server.driver.stickyAttachments.Get(attachment.VolumeHandle); ok {
				return nil
			}
		}
		if r.server.driver.hotplugGuard != nil {
			if _, ok := r.server.hotplugCooldownState(queueCtx, attachment.NodeName); ok {
				return nil
			}
		}
		if _, err := r.server.volumeProvider.GetVolumeInNode(queueCtx, attachment.ImageID, attachment.NodeID); err != nil {
			return nil
		}
		detachStarted := time.Now()
		if err := r.server.volumeProvider.DetachVolume(queueCtx, attachment.VolumeHandle, attachment.NodeName); err != nil {
			r.server.handleHotplugTimeout(queueCtx, attachment.NodeName, attachment.VolumeHandle, nil, "detach", observedAttachmentBackend(attachment), err)
			return err
		}
		r.server.clearHotplugGuardState(queueCtx, attachment.NodeName)
		if sizeBytes, sizeErr := r.server.volumeProvider.ResolveVolumeSizeBytes(queueCtx, attachment.VolumeHandle); sizeErr == nil {
			r.server.driver.observeAdaptiveTimeout(queueCtx, "detach", observedAttachmentBackend(attachment), sizeBytes, time.Since(detachStarted))
		}
		r.server.recordPVCEventForVolumeHandle(queueCtx, attachment.VolumeHandle, eventReasonStuckAttachmentDetached, fmt.Sprintf("detached %s for volume %s from node %s", reason, attachment.VolumeHandle, attachment.NodeName))
		return nil
	})
}

func (r *AttachmentReconciler) recordPVCEventForVolume(ctx context.Context, volumeHandle, reason, message string) {
	if r == nil || r.server == nil {
		return
	}
	r.server.recordPVCEventForVolumeHandle(ctx, volumeHandle, reason, message)
}

func (r *AttachmentReconciler) due(store map[string]time.Time, key string, now time.Time, grace time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	firstSeen, ok := store[key]
	if !ok {
		store[key] = now
		return false
	}
	return now.Sub(firstSeen) >= grace
}

func (r *AttachmentReconciler) prune(store map[string]time.Time, current map[string]struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key := range store {
		if _, ok := current[key]; !ok {
			delete(store, key)
		}
	}
}

func pvcKeyForPV(pv *corev1.PersistentVolume) string {
	if pv == nil || pv.Spec.ClaimRef == nil {
		return ""
	}
	if strings.TrimSpace(pv.Spec.ClaimRef.Namespace) == "" || strings.TrimSpace(pv.Spec.ClaimRef.Name) == "" {
		return ""
	}
	return pv.Spec.ClaimRef.Namespace + "/" + pv.Spec.ClaimRef.Name
}

func observedAttachmentBackend(attachment opennebula.ObservedAttachment) string {
	if strings.TrimSpace(attachment.Backend) != "" {
		return strings.TrimSpace(attachment.Backend)
	}
	return "disk"
}
