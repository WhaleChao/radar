package k8score

import "slices"

// KindReadAssessment contains full membership facts for one cache handle.
// Requested nil means all namespaces; an empty non-nil set means no target.
// Uncovered is known from configuration before sync; Read requires readiness.
// Read and Uncovered must be projected to caller-visible metadata by consumers.
type KindReadAssessment struct {
	Readiness KindReadiness
	Requested []string
	Read      []string
	Uncovered []string
	// AllNamespaces describes the configured informer scope, not the request.
	AllNamespaces bool
	Complete      bool
}

// AssessKindRead assesses an already authorized namespace target without I/O.
// Readiness and coverage describe this receiver, the same cache serving rows.
func (rc *ResourceCache) AssessKindRead(resource string, requested []string) KindReadAssessment {
	assessment := KindReadAssessment{Readiness: rc.KindReadinessFor(resource), Requested: slices.Clone(requested)}
	if assessment.Readiness == KindUnavailable {
		return assessment
	}
	covered := rc.KindNamespaces(resource)
	if covered == nil {
		assessment.AllNamespaces = true
		if assessment.Readiness == KindReady {
			assessment.Read = slices.Clone(requested)
			assessment.Complete = true
		}
		return assessment
	}
	if requested == nil {
		if assessment.Readiness == KindReady {
			assessment.Read = covered
		}
		return assessment
	}
	if assessment.Readiness == KindReady {
		assessment.Read = make([]string, 0, len(requested))
	}
	watched := make(map[string]struct{}, len(covered))
	for _, namespace := range covered {
		watched[namespace] = struct{}{}
	}
	for _, namespace := range requested {
		if _, ok := watched[namespace]; ok {
			if assessment.Readiness == KindReady {
				assessment.Read = append(assessment.Read, namespace)
			}
		} else {
			assessment.Uncovered = append(assessment.Uncovered, namespace)
		}
	}
	assessment.Complete = assessment.Readiness == KindReady && len(assessment.Uncovered) == 0
	return assessment
}
