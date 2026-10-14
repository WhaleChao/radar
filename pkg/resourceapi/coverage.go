// Package resourceapi defines the coverage metadata Radar's resource reads
// share across REST, MCP and search: what an answer is about, what Radar read
// of it, and why anything in it is missing.
package resourceapi

import (
	"cmp"
	"slices"
	"strings"

	"github.com/skyhook-io/radar/pkg/readreason"
)

type CoverageStatus string

const (
	// CoverageComplete: Radar read all of the answer's scope.
	CoverageComplete CoverageStatus = "complete"
	// CoveragePartial: at least one gap; rows prove nothing about the gaps.
	CoveragePartial CoverageStatus = "partial"
)

// Sample caps for the sets below: a REST response is rendered by Radar's UI,
// an MCP response is read by a model.
const (
	RESTSample = 64
	MCPSample  = 8
)

type ScopeType string

const (
	ScopeCluster       ScopeType = "cluster"
	ScopeAllNamespaces ScopeType = "all_namespaces"
	ScopeNamespaces    ScopeType = "namespaces"
)

// Narrowing says why an implicitly broad request covers less than everything.
type Narrowing string

const (
	// NarrowedByCallerAccess: the namespaces the caller may read.
	NarrowedByCallerAccess Narrowing = "caller_access"
	// NarrowedByNamespacePin: Radar's --namespace-scope pin.
	NarrowedByNamespacePin Narrowing = "namespace_pin"
	// NarrowedByNamespaceSelection: the caller's saved namespace selection.
	NarrowedByNamespaceSelection Narrowing = "namespace_selection"
	// NarrowedByCollection: what Radar collects. Only a broad search uses it:
	// it searches what Radar can read and does not list each kind or
	// namespace it skipped. A single-kind read reports collection limits as
	// gaps instead.
	NarrowedByCollection Narrowing = "collection"
)

// NameSet is a known, finite set of names: Count members, the first of them
// (sorted) in Sample. Sample is shorter than Count only because of the cap.
type NameSet struct {
	Count  int      `json:"count"`
	Sample []string `json:"sample"`
}

// Names builds a NameSet from names, which must already be safe to show the
// caller. It does not modify names.
func Names(names []string, limit int) *NameSet {
	names = slices.Clone(names)
	slices.Sort(names)
	names = slices.Compact(names)
	return &NameSet{Count: len(names), Sample: capped(names, limit)}
}

// KindRef names a kind. An omitted group is the core group.
type KindRef struct {
	Kind  string `json:"kind"`
	Group string `json:"group,omitempty"`
}

type KindSet struct {
	Count  int       `json:"count"`
	Sample []KindRef `json:"sample"`
}

func Kinds(kinds []KindRef, limit int) *KindSet {
	kinds = slices.Clone(kinds)
	slices.SortFunc(kinds, func(a, b KindRef) int {
		return cmp.Or(strings.Compare(a.Kind, b.Kind), strings.Compare(a.Group, b.Group))
	})
	kinds = slices.Compact(kinds)
	return &KindSet{Count: len(kinds), Sample: capped(kinds, limit)}
}

// ObjectRef names one object. An omitted group is the core group; an omitted
// namespace is a cluster-scoped object.
type ObjectRef struct {
	Kind      string `json:"kind"`
	Group     string `json:"group,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

type ObjectSet struct {
	Count  int         `json:"count"`
	Sample []ObjectRef `json:"sample"`
}

// Objects builds an ObjectSet of count members from a sample that may already
// be shorter than count.
func Objects(sample []ObjectRef, count, limit int) *ObjectSet {
	return &ObjectSet{Count: max(count, len(sample)), Sample: capped(slices.Clone(sample), limit)}
}

func capped[T any](items []T, limit int) []T {
	items = items[:min(len(items), max(0, limit))]
	if items == nil {
		return []T{}
	}
	return items
}

type Scope struct {
	Type       ScopeType `json:"type"`
	Namespaces *NameSet  `json:"namespaces,omitempty"`
}

// Gap is part of the scope Radar did not read, and why. It covers every
// combination of its present dimensions: kinds × namespaces, or the objects
// named. An absent dimension is not narrowed — the gap may reach any kind or
// namespace of the answer, to an unknown extent — never "none".
type Gap struct {
	Code       readreason.Code `json:"code"`
	Kinds      *KindSet        `json:"kinds,omitempty"`
	Namespaces *NameSet        `json:"namespaces,omitempty"`
	Objects    *ObjectSet      `json:"objects,omitempty"`
}

// Coverage qualifies a single-kind read: a list, or the per-kind entries of a
// count. Status is relative to Scope. An explicit target (namespaces the
// request named) stays the scope, and what of it could not be read is a gap;
// an implicitly broad request's scope is what remained after NarrowedBy, so
// "complete" there means complete within that narrowed scope only. It does
// not assert continuous watch health.
//
// Read is what Radar read within Scope. Every successful answer has it; on an
// error it is absent, which means Radar read nothing.
type Coverage struct {
	Status     CoverageStatus `json:"status"`
	Scope      Scope          `json:"scope"`
	NarrowedBy []Narrowing    `json:"narrowedBy,omitempty"`
	Read       *Scope         `json:"read,omitempty"`
	Gaps       []Gap          `json:"gaps"`
}

// ReadError is the body of a failed resource read on every surface: REST
// responses, and MCP results (structuredContent and the text block). A read
// fails when Radar read nothing in its scope, when the answer would be too
// large, or when its connection changed mid-read. Coverage is present when
// Radar assessed the scope; it keeps every reason, including the ones
// ErrorCode does not name.
type ReadError struct {
	Error     string          `json:"error"`
	ErrorCode readreason.Code `json:"error_code"`
	Coverage  *Coverage       `json:"coverage,omitempty"`
}
