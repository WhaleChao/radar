// Package resourceapi defines resource-read metadata shared by REST and MCP.
package resourceapi

import (
	"slices"

	"github.com/skyhook-io/radar/pkg/readreason"
)

type CoverageStatus string

const (
	CoverageComplete     CoverageStatus = "complete"
	CoveragePartial      CoverageStatus = "partial"
	RESTNamespacePreview                = 64
	MCPNamespacePreview                 = 8
)

type ScopeType string

const (
	ScopeCluster       ScopeType = "cluster"
	ScopeAllNamespaces ScopeType = "all_namespaces"
	ScopeNamespaces    ScopeType = "namespaces"
)

// NamespaceSet describes a known finite set. Omitted counts names left out of
// the metadata preview, never namespaces the collector failed to read.
type NamespaceSet struct {
	Names   []string `json:"names"`
	Total   int      `json:"total"`
	Omitted int      `json:"omitted"`
}

// NamespacePreview must receive an already caller-visible set.
func NamespacePreview(names []string, limit int) *NamespaceSet {
	names = slices.Clone(names)
	slices.Sort(names)
	names = slices.Compact(names)
	total := len(names)
	limit = max(0, limit)
	names = names[:min(total, limit)]
	if names == nil {
		names = []string{}
	}
	return &NamespaceSet{Names: names, Total: total, Omitted: total - len(names)}
}

type Scope struct {
	Type       ScopeType     `json:"type"`
	Namespaces *NamespaceSet `json:"namespaces,omitempty"`
}

type Gap struct {
	Reason readreason.Code `json:"reason"`
	// Nil means the broader set is unknown; it does not mean zero gaps.
	Namespaces *NamespaceSet `json:"namespaces,omitempty"`
}

// Coverage qualifies completeness within the authorized requested scope.
// It does not assert continuous watch health or resource-count exactness.
type Coverage struct {
	Status    CoverageStatus `json:"status"`
	Requested Scope          `json:"requested"`
	Read      Scope          `json:"read"`
	Gaps      []Gap          `json:"gaps"`
}
