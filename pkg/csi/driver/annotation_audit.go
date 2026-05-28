package driver

import "strings"

type AnnotationAuditFinding struct {
	Scope        string `json:"scope"`
	Key          string `json:"key"`
	Value        string `json:"value,omitempty"`
	Severity     string `json:"severity"`
	Message      string `json:"message"`
	CanonicalKey string `json:"canonicalKey,omitempty"`
}

func auditVolumeAnnotations(scope string, annotations map[string]string) []AnnotationAuditFinding {
	findings := []AnnotationAuditFinding{}
	if len(annotations) == 0 {
		return findings
	}
	if value := strings.TrimSpace(annotations[annotationLegacyLastAttachedNode]); value != "" {
		findings = append(findings, AnnotationAuditFinding{
			Scope:        scope,
			Key:          annotationLegacyLastAttachedNode,
			Value:        value,
			Severity:     "warning",
			Message:      "legacy last-attached-node annotation is ignored by the driver",
			CanonicalKey: annotationLastAttachedNode,
		})
	}
	return findings
}
