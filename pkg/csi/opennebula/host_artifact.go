package opennebula

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const HostArtifactConflictClassification = "host_local_lv_conflict"

var hostArtifactLVNamePattern = regexp.MustCompile(`\blv-one-(\d+)-(\d+)\b`)

type HostArtifactAttachmentTarget struct {
	VolumeHandle        string `json:"volumeHandle,omitempty"`
	ImageID             int    `json:"imageID,omitempty"`
	NodeName            string `json:"nodeName,omitempty"`
	VMID                int    `json:"vmID,omitempty"`
	HostID              int    `json:"hostID,omitempty"`
	HostName            string `json:"hostName,omitempty"`
	SystemDatastoreID   int    `json:"systemDatastoreID,omitempty"`
	SystemDatastoreName string `json:"systemDatastoreName,omitempty"`
	SystemDatastoreTM   string `json:"systemDatastoreTM,omitempty"`
	DiskID              int    `json:"diskID,omitempty"`
	Target              string `json:"target,omitempty"`
	LVName              string `json:"lvName,omitempty"`
}

func (t HostArtifactAttachmentTarget) Key() string {
	if t.VMID > 0 && t.DiskID > 0 {
		return fmt.Sprintf("vm-%d.disk-%d", t.VMID, t.DiskID)
	}
	if strings.TrimSpace(t.NodeName) != "" && t.DiskID > 0 {
		return fmt.Sprintf("node-%s.disk-%d", strings.TrimSpace(t.NodeName), t.DiskID)
	}
	if t.VMID > 0 {
		return fmt.Sprintf("vm-%d", t.VMID)
	}
	return strings.TrimSpace(t.NodeName)
}

type HostArtifactAttachmentTargetInspector interface {
	InspectHostArtifactAttachmentTarget(ctx context.Context, volume string, node string, params map[string]string) (*HostArtifactAttachmentTarget, error)
}

type HostArtifactConflictError struct {
	Classification string
	Target         HostArtifactAttachmentTarget
	RawMessage     string
	Cause          error
}

func (e *HostArtifactConflictError) Error() string {
	if e == nil {
		return ""
	}
	classification := strings.TrimSpace(e.Classification)
	if classification == "" {
		classification = HostArtifactConflictClassification
	}
	detail := strings.TrimSpace(e.RawMessage)
	if detail == "" && e.Cause != nil {
		detail = e.Cause.Error()
	}
	fields := []string{
		fmt.Sprintf("classification=%s", classification),
	}
	if e.Target.VMID > 0 {
		fields = append(fields, fmt.Sprintf("vm=%d", e.Target.VMID))
	}
	if strings.TrimSpace(e.Target.NodeName) != "" {
		fields = append(fields, fmt.Sprintf("node=%s", e.Target.NodeName))
	}
	if e.Target.DiskID > 0 {
		fields = append(fields, fmt.Sprintf("disk=%d", e.Target.DiskID))
	}
	if strings.TrimSpace(e.Target.Target) != "" {
		fields = append(fields, fmt.Sprintf("target=%s", e.Target.Target))
	}
	if strings.TrimSpace(e.Target.LVName) != "" {
		fields = append(fields, fmt.Sprintf("lv=%s", e.Target.LVName))
	}
	if detail != "" {
		fields = append(fields, fmt.Sprintf("error=%s", detail))
	}
	return "OpenNebula local host artifact conflict detected: " + strings.Join(fields, " ")
}

func (e *HostArtifactConflictError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func AsHostArtifactConflictError(err error) (*HostArtifactConflictError, bool) {
	var conflict *HostArtifactConflictError
	if errors.As(err, &conflict) && conflict != nil {
		return conflict, true
	}
	return nil, false
}

func HostArtifactConflictFromMessage(raw string, target HostArtifactAttachmentTarget, cause error) (*HostArtifactConflictError, bool) {
	raw = strings.TrimSpace(raw)
	lower := strings.ToLower(raw)
	if raw == "" && cause != nil {
		raw = cause.Error()
		lower = strings.ToLower(raw)
	}
	if raw == "" {
		return nil, false
	}

	if matches := hostArtifactLVNamePattern.FindStringSubmatch(raw); len(matches) == 3 {
		if vmID, err := strconv.Atoi(matches[1]); err == nil && vmID > 0 {
			target.VMID = vmID
		}
		if diskID, err := strconv.Atoi(matches[2]); err == nil && diskID > 0 {
			target.DiskID = diskID
		}
		target.LVName = matches[0]
		return &HostArtifactConflictError{
			Classification: HostArtifactConflictClassification,
			Target:         target,
			RawMessage:     raw,
			Cause:          cause,
		}, true
	}

	hostAttachFailure := strings.Contains(lower, "attachdisk") ||
		strings.Contains(lower, "virsh") ||
		strings.Contains(lower, "logical volume") ||
		strings.Contains(lower, "already exists") ||
		strings.Contains(lower, "lvcreate")
	if !hostAttachFailure {
		return nil, false
	}
	if target.VMID <= 0 || target.DiskID <= 0 {
		return nil, false
	}
	if strings.TrimSpace(target.LVName) == "" {
		target.LVName = fmt.Sprintf("lv-one-%d-%d", target.VMID, target.DiskID)
	}
	return &HostArtifactConflictError{
		Classification: HostArtifactConflictClassification,
		Target:         target,
		RawMessage:     raw,
		Cause:          cause,
	}, true
}
