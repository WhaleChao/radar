package search

import (
	"github.com/skyhook-io/radar/internal/filter"
	"github.com/skyhook-io/radar/pkg/readreason"
	"github.com/skyhook-io/radar/pkg/resourcecontext"
)

const (
	DefaultLimit = 50
	MaxLimit     = 500
)

// CELFilter is the optional structured predicate applied AFTER the
// modifier + free-token match. When set, only candidates that compile
// down to truthy under the CEL program are kept; eval errors count as
// a non-match (consistent with "missing field" semantics).
type CELFilter = filter.Filter

// Query is a parsed search request.
//
// Free Tokens are AND'd: a hit must match every token in at least one site.
// Modifiers (KindFilter, NSFilter, LabelFilter, ImageFilter) act as hard
// boolean filters — a candidate that doesn't satisfy them is excluded
// before scoring.
type Query struct {
	Tokens      []string  // free tokens; each must match somewhere; sum of best per-site scores = total
	KindFilter  []string  // kind:Foo modifiers as written; matches Kind name (singular or plural), case-insensitively
	NSFilter    []string  // ns:foo modifiers; restrict namespaced kinds, and exclude cluster-scoped kinds unless a kind: names them
	LabelFilter []LabelEq // label:k=v modifiers; AND'd
	ImageFilter []string  // image:foo modifiers; substring match on container images
	Cluster     string    // cluster: modifier — radar ignores; hub uses for routing
	Raw         string    // original query string, for debugging / round-tripping
}

// LabelEq is a single key=value label filter parsed from "label:k=v".
type LabelEq struct {
	Key   string
	Value string
}

// Hit is a single ranked search result.
type Hit struct {
	Score     int            `json:"score"`
	Cluster   string         `json:"cluster,omitempty"`
	Kind      string         `json:"kind"`
	Group     string         `json:"group,omitempty"`
	Namespace string         `json:"namespace,omitempty"`
	Name      string         `json:"name"`
	Summary   any            `json:"summary,omitempty"`
	Raw       any            `json:"raw,omitempty"`
	Matched   []MatchedField `json:"matched,omitempty"`
	Snippets  []MatchSnippet `json:"snippets,omitempty"`
	// SummaryContext is the compact per-row enrichment (managedBy, health,
	// issueCount). Populated by handlers via Options.SummaryBuilder; nil
	// when the caller opted out (context=none) or no fields apply.
	SummaryContext *resourcecontext.ResourceSummaryContext `json:"summaryContext,omitempty"`
}

// MatchedField records where a query token landed (debug + UI highlight).
type MatchedField struct {
	Token string `json:"token"`
	Site  string `json:"site"` // "name" | "namespace" | "label:k" | "annotation:k" | "image" | "kind" | "content:path"
	Score int    `json:"score"`
}

// MatchSnippet is a short excerpt from a content field that matched a free
// token. It lets agents use search as a cheap grep-like first pass without
// fetching the full resource body for every hit.
type MatchSnippet struct {
	Token   string `json:"token"`
	Path    string `json:"path"`
	Snippet string `json:"snippet"`
}

// UnsearchedKind is one coverage entry. Kind-level entries name a kind; "*"
// entries either stand for several kinds or carry Namespaces. Namespaces is set
// only for namespace_excluded, namespace_scope_limited and
// collector_scope_limited, and lists the namespaces the query named that were
// not searched for that reason. Count is set only on a "*" entry that folds
// kinds: how many it stands for.
type UnsearchedKind struct {
	Kind       string          `json:"kind"`
	Group      string          `json:"group"`
	Reason     readreason.Code `json:"reason"`
	Namespaces []string        `json:"namespaces,omitempty"`
	Count      int             `json:"count,omitempty"`
	// RetryWithKind distinguishes a deferred watch-start failure from a stalled
	// watch: only the former can be restarted by an explicit kind search.
	RetryWithKind bool `json:"retryWithKind,omitempty"`
}

type FailedObject struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// Result is the full response shape for a search request.
type Result struct {
	// Partial: true means this result can't rule out a match for what you
	// asked: something was loading or failed, or a kind or namespace you named
	// couldn't be searched. false means no such problem was reported; it is
	// not proof that nothing exists outside your access or Radar's collection.
	// Result limits (Total vs TotalMatched) are separate.
	Partial             bool             `json:"partial"`
	Unsearched          []UnsearchedKind `json:"unsearched"`
	FilterFailedObjects []FailedObject   `json:"filter_failed_objects,omitempty"`
	Hits                []Hit            `json:"hits"`
	Total               int              `json:"total"`    // number of hits returned (after limit)
	Searched            int              `json:"searched"` // approx. number of objects scanned
	// TotalMatched is the count of hits BEFORE truncation by Limit.
	// Equals Total when no truncation occurred. Surfaced so callers
	// (the hub's fleet aggregator, agents, the frontend) can report honest
	// "X of N" counts when the limit clips the result set — without
	// it the caller has no way to tell it's looking at a windowed view.
	TotalMatched int `json:"total_matched"`
	// FilterErrors counts rows the CEL filter rejected with an eval
	// error (not parse — parse errors fail compile and return 400).
	// Zero on a healthy filter; non-zero means some rows had missing
	// fields or type mismatches the filter assumed away. Agents
	// reading this can self-correct: "filter excluded everything" vs
	// "cluster has nothing matching" become distinguishable.
	FilterErrors int `json:"filter_errors,omitempty"`
	// FilterErrorSample is the first eval error message verbatim so
	// the caller has an actionable hint. Empty when FilterErrors=0.
	FilterErrorSample string `json:"filter_error_sample,omitempty"`
}

// IncludeMode controls per-hit verbosity.
type IncludeMode int

const (
	IncludeSummary IncludeMode = iota
	IncludeRaw
	IncludeNone // identity only (cheapest)
)
