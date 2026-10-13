package k8score

import "slices"

// KindReadAssessment contains full membership facts for one cache handle.
// Requested nil means all namespaces; an empty non-nil set means no target.
// Read and Uncovered must be projected to caller-visible metadata by consumers.
type KindReadAssessment struct {
	Readiness     KindReadiness
	Requested     []string
	Read          []string
	Uncovered     []string
	AllNamespaces bool
	Complete      bool
}

// AssessKindRead assesses an already authorized namespace target without I/O.
// Readiness and coverage describe this receiver, the same cache serving rows.
func (rc *ResourceCache) AssessKindRead(resource string, requested []string) KindReadAssessment {
	assessment := KindReadAssessment{Readiness: rc.KindReadinessFor(resource), Requested: slices.Clone(requested)}
	if assessment.Readiness != KindReady {
		return assessment
	}
	covered := rc.KindNamespaces(resource)
	if covered == nil {
		assessment.AllNamespaces = true
		assessment.Read = slices.Clone(requested)
		assessment.Complete = true
		return assessment
	}
	if requested == nil {
		assessment.Read = covered
		return assessment
	}
	assessment.Read = make([]string, 0, len(requested))
	watched := make(map[string]struct{}, len(covered))
	for _, namespace := range covered {
		watched[namespace] = struct{}{}
	}
	for _, namespace := range requested {
		if _, ok := watched[namespace]; ok {
			assessment.Read = append(assessment.Read, namespace)
		} else {
			assessment.Uncovered = append(assessment.Uncovered, namespace)
		}
	}
	assessment.Complete = len(assessment.Uncovered) == 0
	return assessment
}
