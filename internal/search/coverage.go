package search

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/skyhook-io/radar/internal/k8s"
	"github.com/skyhook-io/radar/pkg/k8score"
	"github.com/skyhook-io/radar/pkg/readreason"
)

// NamespacedSearchKinds lists the sensitive typed kinds requiring exact caller RBAC.
var NamespacedSearchKinds = []struct{ Kind, Group, Resource string }{
	{"Secret", "", "secrets"},
	{"Role", "rbac.authorization.k8s.io", "roles"},
	{"RoleBinding", "rbac.authorization.k8s.io", "rolebindings"},
}

// reasonClass groups coverage reasons by how a search reports them.
type reasonClass int

const (
	// classScope covers the caller's access, Radar's setup and query feedback.
	// They describe what a search covers rather than what went wrong, so they
	// are reported only for a kind or namespace the query names.
	classScope reasonClass = iota
	// classTransient is data still loading; a retry can change the answer.
	classTransient
	// classFailure is a read or check that failed for a kind the caller may read.
	classFailure
)

func classify(reason readreason.Code) reasonClass {
	switch reason {
	case readreason.KindSyncPending:
		return classTransient
	case readreason.KindSyncFailed, readreason.ReadError, readreason.PermissionCheckFailed, readreason.CollectorUnauthorized:
		return classFailure
	}
	return classScope
}

// maxNamedFailures bounds the kinds one failure reason names on a query with
// no kind: terms; the rest fold into that reason's "*" entry.
const maxNamedFailures = 5

// coverage collects one search's unsearched entries and applies the reporting
// rules: scope reasons only for what the query names, every transient and
// failure reason, folded so the response never grows with the number of kinds
// on the cluster.
type coverage struct {
	namedKinds      bool
	namedNamespaces map[string]bool
	namespaceIndex  map[readreason.Code]map[string]bool
	gaps            []UnsearchedKind
	gapIndex        map[coverageKey]int
	// unbounded marks "*" entries standing for an unknown number of kinds (the
	// catalog itself could not be read); they carry no count.
	unbounded map[readreason.Code]bool
}

type coverageKey struct {
	kind, group string
	reason      readreason.Code
}

func newCoverage(q Query) *coverage {
	namespaces := map[string]bool{}
	for _, ns := range q.NSFilter {
		namespaces[ns] = true
	}
	return &coverage{
		namedKinds:      len(q.KindFilter) > 0,
		namedNamespaces: namespaces,
		namespaceIndex:  map[readreason.Code]map[string]bool{},
		unbounded:       map[readreason.Code]bool{},
		gapIndex:        map[coverageKey]int{},
	}
}

// quiet reports whether a kind-level reason goes unreported for this query,
// so callers can skip work that could only produce it.
func (c *coverage) quiet(reason readreason.Code) bool {
	return classify(reason) == classScope && !c.namedKinds
}

// kind records a kind-level reason for one scanned kind.
func (c *coverage) kind(kind, group string, reason readreason.Code) *UnsearchedKind {
	if c.quiet(reason) {
		return nil
	}
	key := coverageKey{kind, group, reason}
	if i, exists := c.gapIndex[key]; exists {
		return &c.gaps[i]
	}
	c.gapIndex[key] = len(c.gaps)
	c.gaps = append(c.gaps, UnsearchedKind{Kind: kind, Group: group, Reason: reason})
	return &c.gaps[len(c.gaps)-1]
}

// catalog records a reason that applies to an unknown set of kinds, such as
// discovery not being ready.
func (c *coverage) catalog(reason readreason.Code) {
	if c.quiet(reason) {
		return
	}
	c.unbounded[reason] = true
	c.star(reason)
}

// namespaces records a namespace-level reason. Only namespaces the query names
// are reported, merged into one entry per reason.
func (c *coverage) namespaces(reason readreason.Code, namespaces ...string) {
	var entry *UnsearchedKind
	seen := c.namespaceIndex[reason]
	for _, ns := range namespaces {
		if !c.namedNamespaces[ns] || seen[ns] {
			continue
		}
		if entry == nil {
			entry = c.star(reason)
		}
		if seen == nil {
			seen = map[string]bool{}
			c.namespaceIndex[reason] = seen
		}
		seen[ns] = true
		entry.Namespaces = append(entry.Namespaces, ns)
	}
}

func (c *coverage) star(reason readreason.Code) *UnsearchedKind {
	key := coverageKey{kind: "*", reason: reason}
	if i, exists := c.gapIndex[key]; exists {
		return &c.gaps[i]
	}
	c.gapIndex[key] = len(c.gaps)
	c.gaps = append(c.gaps, UnsearchedKind{Kind: "*", Reason: reason})
	return &c.gaps[len(c.gaps)-1]
}

// result folds the recorded entries. On a query that names kinds, every
// kind-level entry is one of them and stays exact. Otherwise pending kinds
// become one counted "*" entry, and each failure reason names its first
// maxNamedFailures kinds by kind then group and counts the rest in its "*"
// entry.
func (c *coverage) result() []UnsearchedKind {
	out := []UnsearchedKind{}
	pending := map[[2]string]bool{}
	failures := map[readreason.Code][]UnsearchedKind{}
	for _, gap := range c.gaps {
		slices.Sort(gap.Namespaces)
		switch class := classify(gap.Reason); {
		case gap.Kind == "*" || c.namedKinds || class == classScope:
			out = append(out, gap)
		case class == classTransient:
			pending[[2]string{gap.Kind, gap.Group}] = true
		default:
			failures[gap.Reason] = append(failures[gap.Reason], gap)
		}
	}
	fold := func(reason readreason.Code, n int) {
		for i := range out {
			if out[i].Kind == "*" && out[i].Reason == reason {
				if !c.unbounded[reason] {
					out[i].Count += n
				}
				return
			}
		}
		out = append(out, UnsearchedKind{Kind: "*", Reason: reason, Count: n})
	}
	if len(pending) > 0 {
		fold(readreason.KindSyncPending, len(pending))
	}
	for reason, gaps := range failures {
		sortGaps(gaps)
		out = append(out, gaps[:min(len(gaps), maxNamedFailures)]...)
		if len(gaps) > maxNamedFailures {
			fold(reason, len(gaps)-maxNamedFailures)
		}
	}
	sortGaps(out)
	return out
}

func sortGaps(gaps []UnsearchedKind) {
	slices.SortFunc(gaps, func(a, b UnsearchedKind) int {
		return cmp.Or(strings.Compare(a.Kind, b.Kind), strings.Compare(a.Group, b.Group), strings.Compare(string(a.Reason), string(b.Reason)))
	})
}

// ScopeNamespaces sets the namespace fields of o for q. visible is what the
// caller may search after any namespace selection (a saved pick or the
// --namespace-scope pin): nil means every namespace, empty means none. ceiling
// is the caller's RBAC visibility without that selection. A namespace an ns:
// term names but visible leaves out is the caller's RBAC when ceiling excludes
// it too, and the selection otherwise.
func (o *Options) ScopeNamespaces(q Query, visible, ceiling []string) {
	o.Namespaces = visible
	o.ExcludedNamespaces = nil
	o.SelectionExcludedNamespaces = nil
	o.seesAllNamespaces = ceiling == nil
	if len(q.NSFilter) > 0 {
		o.Namespaces = make([]string, 0, len(q.NSFilter))
		for _, ns := range q.NSFilter {
			into := &o.ExcludedNamespaces
			switch {
			case visible == nil || slices.Contains(visible, ns):
				into = &o.Namespaces
			case ceiling == nil || slices.Contains(ceiling, ns):
				into = &o.SelectionExcludedNamespaces
			}
			if !slices.Contains(*into, ns) {
				*into = append(*into, ns)
			}
		}
	}
	o.NamespaceExcluded = o.Namespaces != nil && len(o.Namespaces) == 0
}

// excludeMissingNamespaces reports a namespace an ns: term names that doesn't
// exist on the cluster as namespace_excluded, the entry an unseen namespace
// gets, instead of searching it to a silent empty answer or blaming the
// namespace selection. Only for a caller whose RBAC covers every namespace:
// for anyone else, which namespaces exist is not theirs to learn.
func (o *Options) excludeMissingNamespaces(p Provider, q Query) {
	if len(q.NSFilter) == 0 || !o.seesAllNamespaces {
		return
	}
	split := func(namespaces []string) (kept []string) {
		if namespaces == nil {
			return nil
		}
		kept = make([]string, 0, len(namespaces))
		for _, ns := range namespaces {
			if exists, known := p.NamespaceExists(ns); known && !exists {
				o.ExcludedNamespaces = append(o.ExcludedNamespaces, ns)
			} else {
				kept = append(kept, ns)
			}
		}
		return kept
	}
	o.Namespaces = split(o.Namespaces)
	o.SelectionExcludedNamespaces = split(o.SelectionExcludedNamespaces)
	o.NamespaceExcluded = o.Namespaces != nil && len(o.Namespaces) == 0
}

// errIncompleteDiscovery marks a dynamic catalog missing the API groups whose
// discovery failed.
var errIncompleteDiscovery = errors.New("some API groups could not be discovered")

// listErrorReason classifies a failed collector list or warm-up. The typed
// cache reports a probe-disabled kind as a "forbidden:" error, and
// ErrUnknownKind marks a kind Radar reads directly instead of watching. A 401
// stays collector_access_denied: the probes and retained observations behind
// most collector denials do not keep 401 apart from 403.
func listErrorReason(err error) readreason.Code {
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || strings.HasPrefix(err.Error(), "forbidden:") {
		return readreason.CollectorAccessDenied
	}
	if errors.Is(err, k8s.ErrUnknownKind) {
		return readreason.CollectionNotSupported
	}
	if errors.Is(err, k8s.ErrDynamicNotReady) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || k8score.IsClientRateLimited(err) {
		return readreason.KindSyncPending
	}
	return readreason.ReadError
}

func dynamicObservationReason(observation k8score.DynamicResourceObservation) readreason.Code {
	if observation.ProbeRetrying {
		return readreason.KindSyncPending
	}
	if len(observation.UnansweredNamespaces) > 0 || observation.ReasonCode == "scope_probe_incomplete" {
		return readreason.ReadError
	}
	switch observation.State {
	case k8score.DynamicObservationSynced:
		return ""
	case k8score.DynamicObservationDenied:
		return readreason.CollectorAccessDenied
	case k8score.DynamicObservationSyncing:
		if observation.ReasonCode == "sync_stalled" {
			return readreason.KindSyncFailed
		}
		return readreason.KindSyncPending
	case k8score.DynamicObservationDeferred:
		switch observation.ReasonCode {
		case "eager_watch_pending":
			return readreason.KindSyncPending
		case "watch_start_failed":
			return readreason.KindSyncFailed
		}
		return readreason.KindNotWatched
	case k8score.DynamicObservationUnsupported:
		return readreason.CollectionNotSupported
	default:
		return readreason.KindNotWatched
	}
}
