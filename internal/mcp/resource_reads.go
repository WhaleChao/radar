package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/skyhook-io/radar/internal/k8s"
	pkgauth "github.com/skyhook-io/radar/pkg/auth"
	"github.com/skyhook-io/radar/pkg/k8score"
	"github.com/skyhook-io/radar/pkg/readreason"
	"github.com/skyhook-io/radar/pkg/resourceapi"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// readError is a resource-read failure with the code an agent branches on.
// Its Go error text is "<code>: <explanation>"; readErrorResult sends it to
// MCP clients as a resourceapi.ReadError body. unread, when the failure was
// assessed namespace by namespace, becomes the body's coverage.
type readError struct {
	code   readreason.Code
	err    error
	unread *gapSet
	// coverage is attached by a list, which knows the read's scope.
	coverage *resourceapi.Coverage
}

func (e *readError) Error() string { return string(e.code) + ": " + e.err.Error() }
func (e *readError) Unwrap() error { return e.err }

func readErrorf(code readreason.Code, format string, args ...any) error {
	return &readError{code: code, err: fmt.Errorf(format, args...)}
}

// readErrorResult renders a readError as an MCP tool error. The body goes in
// both structuredContent and the text block: clients that read only text —
// including Radar Hub, which records the text as investigation evidence —
// must get the same code and coverage. It reports false for any other error,
// which keeps the SDK's text-only form.
func readErrorResult(err error) (*mcp.CallToolResult, bool) {
	var re *readError
	if !errors.As(err, &re) {
		return nil, false
	}
	body := resourceapi.ReadError{Error: re.err.Error(), ErrorCode: re.code, Coverage: re.coverage}
	text, marshalErr := json.Marshal(body)
	if marshalErr != nil {
		return nil, false
	}
	result := &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(text)}},
		StructuredContent: body,
	}
	result.SetError(err)
	return result, true
}

// errConnectionChanged rejects a read whose cache, dynamic cache or discovery
// handle was replaced while it ran: its rows may mix two clusters, and a retry
// may reach a different one.
func errConnectionChanged() error {
	return readErrorf(readreason.ConnectionChanged, "Radar's cluster connection changed during this read (a context switch or reconnect); confirm which cluster Radar is connected to, then retry")
}

// listCoverage is list_resources' coverage: the coverage every resource read
// shares, plus a sentence for the model reading the result.
type listCoverage struct {
	resourceapi.Coverage
	Note string `json:"note,omitempty"`
}

// listEnvelope is every list_resources result, sent as the only text block.
// Findings parses the text agent backends hand it, and the evidence ledger
// matches that text against the producer's exactly, so the rows and their
// coverage must be one JSON value: a second block would be joined into
// something neither accepts. Coverage leads so a reader that keeps only the
// head of a long result still has the caveat.
type listEnvelope struct {
	Coverage listCoverage `json:"coverage"`
	Items    []any        `json:"items"`
}

func listResult(items []any, coverage listCoverage) (*mcp.CallToolResult, any, error) {
	if items == nil {
		items = []any{}
	}
	body, err := json.Marshal(listEnvelope{Coverage: coverage, Items: items})
	if err != nil {
		log.Printf("[mcp] Failed to marshal result: %v", err)
		return nil, nil, fmt.Errorf("failed to marshal result: %w", err)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(body)}}}, nil, nil
}

// readTarget is what a namespaced read is about: the namespace the request
// named, or what remained of "every namespace" after Radar's --namespace-scope
// pin and the caller's access narrowed it. A named namespace stays the target
// even when it can't be read, so what's missing is a gap; implicit narrowing
// is declared instead, and "complete" then means complete within it.
type readTarget struct {
	namespaces []string // nil: every namespace
	explicit   bool
	narrowedBy []resourceapi.Narrowing
}

func resolveReadTarget(ctx context.Context, requested []string) (readTarget, error) {
	target := readTarget{explicit: requested != nil}
	clamped, ok := clampToNamespacePin(requested)
	if !ok {
		return target, readErrorf(readreason.OutsideNamespaceScope, "requested namespace %q is excluded by Radar's --namespace-scope selection", namespaceForError(requested))
	}
	if !target.explicit && clamped != nil {
		target.narrowedBy = append(target.narrowedBy, resourceapi.NarrowedByNamespacePin)
	}
	user, perms, checked := resolveUserPermsDecision(ctx)
	if !checked {
		return target, readErrorf(readreason.PermissionCheckFailed, "could not discover your Radar namespace access; retry when the permission check is available")
	}
	allowed := pkgauth.FilterNamespacesForUser(clamped, user, perms)
	if allowed != nil && len(allowed) == 0 {
		// Name only what the caller asked for: unscoped, clamped is the
		// --namespace-scope target, which this caller cannot read.
		scope := "any namespace in the requested scope"
		if len(requested) == 1 {
			scope = fmt.Sprintf("namespace %q", clamped[0])
		} else if len(requested) > 1 {
			scope = fmt.Sprintf("namespaces %q", namespaceForError(clamped))
		}
		return target, readErrorf(readreason.NoNamespaceAccess, "no Radar access to %s (Radar grants namespace access when your role can list pods or deployments there; re-checked every ~2 minutes)", scope)
	}
	if !target.explicit && clamped == nil && allowed != nil {
		target.narrowedBy = append(target.narrowedBy, resourceapi.NarrowedByCallerAccess)
	}
	target.namespaces = allowed
	return target, nil
}

// resourceReadNamespaces returns the namespaces a read of requested may reach
// for this caller: nil for every namespace.
func resourceReadNamespaces(ctx context.Context, requested []string) ([]string, error) {
	target, err := resolveReadTarget(ctx, requested)
	return target.namespaces, err
}

func namespaceForError(namespaces []string) string {
	if len(namespaces) == 1 {
		return namespaces[0]
	}
	if len(namespaces) > 3 {
		return fmt.Sprintf("%s and %d more", strings.Join(namespaces[:3], ","), len(namespaces)-3)
	}
	return strings.Join(namespaces, ",")
}

func resourcePermissionError(checked bool, verb, group, resource, namespace string) error {
	scope := "at cluster scope"
	if strings.Contains(namespace, ",") {
		scope = fmt.Sprintf("in namespaces %q", namespace)
	} else if namespace != "" {
		scope = fmt.Sprintf("in namespace %q", namespace)
	}
	if !checked {
		return readErrorf(readreason.PermissionCheckFailed, "could not verify whether your role can %s resource %q in API group %q %s; access was not granted, retry when the permission check is available", verb, resource, group, scope)
	}
	return readErrorf(readreason.RBACDenied, "your role cannot %s resource %q in API group %q %s; ask a cluster administrator to grant access", verb, resource, group, scope)
}

// gapSet collects the namespaces a read could not serve, by code. A code
// recorded with no namespaces reaches an unknown part of the scope.
type gapSet struct {
	namespaces map[readreason.Code][]string
	unknown    map[readreason.Code]bool
	firstErr   map[readreason.Code]error
}

func (g *gapSet) add(code readreason.Code, err error, namespaces ...string) {
	if g.namespaces == nil {
		g.namespaces, g.unknown, g.firstErr = map[readreason.Code][]string{}, map[readreason.Code]bool{}, map[readreason.Code]error{}
	}
	if len(namespaces) == 0 {
		g.unknown[code] = true
	}
	g.namespaces[code] = append(g.namespaces[code], namespaces...)
	if _, ok := g.firstErr[code]; !ok && err != nil {
		g.firstErr[code] = err
	}
}

func (g *gapSet) merge(other gapSet) {
	for code, namespaces := range other.namespaces {
		g.add(code, other.firstErr[code], namespaces...)
		if other.unknown[code] {
			g.unknown[code] = true
		}
	}
}

func (g *gapSet) empty() bool { return len(g.namespaces) == 0 }

// absorb adds a failed read's gaps to g: its assessed gaps, or its code over
// an unknown extent. It reports false for an error that isn't a read failure.
func (g *gapSet) absorb(err error) bool {
	var re *readError
	if !errors.As(err, &re) {
		return false
	}
	if re.unread != nil {
		g.merge(*re.unread)
	} else {
		g.add(re.code, err)
	}
	return true
}

func (g *gapSet) codes() []readreason.Code {
	codes := make([]readreason.Code, 0, len(g.namespaces))
	for code := range g.namespaces {
		codes = append(codes, code)
	}
	slices.SortFunc(codes, func(a, b readreason.Code) int {
		switch {
		case a == b:
			return 0
		case readreason.Primary([]readreason.Code{a, b}) == a:
			return -1
		default:
			return 1
		}
	})
	return codes
}

// gaps renders the set, most actionable first. Namespaces are projected to
// what the caller may see; a code whose every namespace is hidden keeps its
// gap with no namespaces, because the gap is real but its extent is not
// theirs to learn.
func (g *gapSet) gaps(ctx context.Context, limit int) []resourceapi.Gap {
	out := []resourceapi.Gap{}
	for _, code := range g.codes() {
		gap := resourceapi.Gap{Code: code}
		if !g.unknown[code] {
			if visible := callerVisibleNamespaces(ctx, g.namespaces[code]); len(visible) > 0 {
				gap.Namespaces = resourceapi.Names(visible, limit)
			}
		}
		out = append(out, gap)
	}
	return out
}

// failure is the error for a read that served nothing: the most actionable
// code, its own explanation, and every gap.
func (g *gapSet) failure() error {
	code := readreason.Primary(g.codes())
	var re *readError
	if errors.As(g.firstErr[code], &re) {
		return &readError{code: re.code, err: re.err, unread: g}
	}
	return &readError{code: code, err: errors.New("Radar could not read any of the requested scope"), unread: g}
}

// listScope is the scope a list's coverage reports for target.
func listScope(target readTarget, clusterScoped bool) resourceapi.Scope {
	switch {
	case clusterScoped:
		return resourceapi.Scope{Type: resourceapi.ScopeCluster}
	case target.namespaces == nil:
		return resourceapi.Scope{Type: resourceapi.ScopeAllNamespaces}
	default:
		return resourceapi.Scope{Type: resourceapi.ScopeNamespaces, Namespaces: resourceapi.Names(target.namespaces, resourceapi.MCPSample)}
	}
}

// listCoverageFor assembles a successful list's coverage. read is what Radar
// read; readAll means the whole scope.
func listCoverageFor(resource string, clusterScoped bool, target readTarget, read []string, readAll bool, gaps []resourceapi.Gap) listCoverage {
	cov := resourceapi.Coverage{
		Status: resourceapi.CoverageComplete,
		Scope:  listScope(target, clusterScoped),
		Gaps:   gaps,
	}
	if !clusterScoped {
		cov.NarrowedBy = target.narrowedBy
	}
	readScope := cov.Scope
	if !readAll && !clusterScoped {
		readScope = resourceapi.Scope{Type: resourceapi.ScopeNamespaces, Namespaces: resourceapi.Names(read, resourceapi.MCPSample)}
	}
	cov.Read = &readScope
	if len(gaps) > 0 {
		cov.Status = resourceapi.CoveragePartial
	}
	if cov.Gaps == nil {
		cov.Gaps = []resourceapi.Gap{}
	}
	return listCoverage{Coverage: cov, Note: listNote(resource, cov)}
}

// withListCoverage attaches a failed list's coverage: its scope and every gap,
// with no read scope because Radar read nothing.
func withListCoverage(ctx context.Context, err error, target readTarget, clusterScoped bool) error {
	var re *readError
	if !errors.As(err, &re) || re.unread == nil {
		return err
	}
	cov := &resourceapi.Coverage{Status: resourceapi.CoveragePartial, Scope: listScope(target, clusterScoped), Gaps: re.unread.gaps(ctx, resourceapi.MCPSample)}
	if !clusterScoped {
		cov.NarrowedBy = target.narrowedBy
	}
	return &readError{code: re.code, err: re.err, unread: re.unread, coverage: cov}
}

// gapPhrases says why a namespace went unread, in the words of a list's note.
var gapPhrases = map[readreason.Code]string{
	readreason.CollectorScopeLimited: "outside the namespaces Radar collects",
	readreason.CollectorForbidden:    "Radar's own identity was refused there",
	readreason.KindSyncPending:       "still loading",
	readreason.KindSyncFailed:        "did not finish loading",
	readreason.KindWatchFailed:       "the watch failed to start",
	readreason.RBACDenied:            "your access does not include them",
	readreason.PermissionCheckFailed: "your access could not be checked",
}

func namespacePhrase(set *resourceapi.NameSet) string {
	quoted := make([]string, len(set.Sample))
	for i, ns := range set.Sample {
		quoted[i] = fmt.Sprintf("%q", ns)
	}
	list := strings.Join(quoted, ", ")
	if more := set.Count - len(set.Sample); more > 0 {
		list += fmt.Sprintf(" and %d more", more)
	}
	if set.Count == 1 {
		return "namespace " + list
	}
	return "namespaces " + list
}

// listNote is the sentence a model reads before a list's rows. It says what
// the rows cover whenever that is less than the whole cluster, and never
// claims anything about what was not read.
func listNote(resource string, cov resourceapi.Coverage) string {
	var b strings.Builder
	if len(cov.NarrowedBy) > 0 && cov.Scope.Namespaces != nil {
		why := "your access"
		if slices.Contains(cov.NarrowedBy, resourceapi.NarrowedByNamespacePin) {
			why = "Radar's --namespace-scope pin"
		}
		fmt.Fprintf(&b, "This list covers only %s (%s). ", namespacePhrase(cov.Scope.Namespaces), why)
	}
	if cov.Status != resourceapi.CoveragePartial {
		if b.Len() == 0 {
			return ""
		}
		fmt.Fprintf(&b, "It says nothing about %s in other namespaces.", resource)
		return b.String()
	}
	if cov.Read != nil && cov.Read.Namespaces != nil {
		fmt.Fprintf(&b, "Radar read %s only in %s. ", resource, namespacePhrase(cov.Read.Namespaces))
	}
	var parts []string
	for _, gap := range cov.Gaps {
		phrase := gapPhrases[gap.Code]
		if phrase == "" {
			phrase = string(gap.Code)
		}
		where := "other namespaces"
		if gap.Namespaces != nil {
			where = namespacePhrase(gap.Namespaces)
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", where, phrase))
	}
	fmt.Fprintf(&b, "Not read: %s. This result cannot show that %s there don't exist.", strings.Join(parts, "; "), resource)
	return b.String()
}

func resourceReadCacheIsCurrent(cache *k8s.ResourceCache) bool {
	active, syncing := k8s.SnapshotCaches()
	if active != nil {
		return cache == active
	}
	return cache != nil && cache == syncing
}

// typedRead is what a typed kind's cache can serve of a target now.
type typedRead struct {
	resource string
	// all: the target is every namespace and all of it is readable.
	all bool
	// read: the namespaces to read when !all; non-nil.
	read []string
	// whole: the kind is ready as a whole, so its listers serve the read. A
	// kind still loading elsewhere is read from its namespaces' own stores.
	whole bool
	gaps  gapSet
}

// resourceReadCache returns the cache to read kind from and, for a typed kind,
// what of namespaces (nil: every namespace) it can serve now and why the rest
// is unread. It fails only when nothing can be read. Dynamic kinds learn their
// coverage by reading, so they get no plan.
func resourceReadCache(ctx context.Context, kind, group string, namespaces []string) (*k8s.ResourceCache, *typedRead, error) {
	gvr, builtin := k8s.BuiltinGVRAnyGroup(kind)
	typed := builtin && (group == "" || group == gvr.Group) && k8s.TypedKindOwnsGroup(kind, gvr.Group)
	if !typed {
		cache := k8s.GetResourceCache()
		if cache == nil {
			return nil, nil, errNotConnected()
		}
		return cache, nil, nil
	}
	cache, syncing := k8s.SnapshotCaches()
	if cache == nil {
		cache = syncing
	}
	if cache == nil {
		return nil, nil, errNotConnected()
	}
	result := cache.PermissionResult()
	if result != nil {
		for ns, probeErr := range result.NamespaceProbeErrors[gvr.Resource] {
			if namespaces != nil && !slices.Contains(namespaces, ns) {
				continue
			}
			if apierrors.IsUnauthorized(probeErr) {
				return nil, nil, collectorUnauthorizedError(gvr.Resource, probeErr)
			}
		}
	}
	a := cache.AssessKindRead(gvr.Resource, namespaces)
	if a.Readiness == k8s.KindUnavailable {
		if result != nil {
			if scope, probed := result.Scopes[gvr.Resource]; probed && !scope.Enabled {
				if apierrors.IsUnauthorized(result.ProbeErrors[gvr.Resource]) {
					return nil, nil, collectorUnauthorizedError(gvr.Resource, result.ProbeErrors[gvr.Resource])
				}
				return nil, nil, readErrorf(readreason.CollectorForbidden, "%s can't list/watch %s in API group %q%s", collectorName(), gvr.Resource, gvr.Group, collectorRemedy(ctx, gvr.Resource))
			}
		}
		return nil, nil, readErrorf(readreason.KindNotWatched, "Radar is not watching resource %q in API group %q; check Radar's collector list/watch permissions and collection configuration", gvr.Resource, gvr.Group)
	}
	pending := readErrorf(readreason.KindSyncPending, "%s are still loading, please retry shortly", gvr.Resource)
	failed := readErrorf(readreason.KindSyncFailed, "%s did not finish loading within the sync deadline; Radar keeps trying and they may still finish, so retry later, and if it persists check Radar's collector connection and list/watch permissions", gvr.Resource)
	plan := &typedRead{resource: gvr.Resource, whole: a.Readiness == k8s.KindReady}
	if a.AllNamespaces && namespaces == nil {
		switch a.Readiness {
		case k8s.KindReady:
			plan.all = true
			return cache, plan, nil
		case k8s.KindFailed:
			plan.gaps.add(readreason.KindSyncFailed, failed)
		default:
			plan.gaps.add(readreason.KindSyncPending, pending)
		}
		return nil, nil, plan.gaps.failure()
	}
	target := collectorTarget{resource: gvr.Resource, group: gvr.Group, action: "list/watch"}
	if len(a.Uncovered) > 0 {
		plan.gaps.add(readreason.CollectorScopeLimited, coverageError(ctx, target, a.Uncovered, cache.KindNamespaces(gvr.Resource), nil), a.Uncovered...)
	}
	if namespaces == nil && !a.AllNamespaces {
		plan.gaps.add(readreason.CollectorScopeLimited, coverageError(ctx, target, nil, a.Read, nil))
	}
	if len(a.Loading) > 0 {
		plan.gaps.add(readreason.KindSyncPending, pending, a.Loading...)
	}
	if len(a.Failed) > 0 {
		plan.gaps.add(readreason.KindSyncFailed, failed, a.Failed...)
	}
	if len(a.Read) == 0 {
		return nil, nil, plan.gaps.failure()
	}
	plan.read = a.Read
	return cache, plan, nil
}

// collectorTarget is what Radar's collector reads for a kind: the resource,
// its API group, and the action it performs (list/watch for a cache, or the
// verb of a direct read).
type collectorTarget struct {
	resource, group, action string
}

func dynamicCollectorTarget(kind, group, verb string) collectorTarget {
	gvr, ok := dynamicReadGVR(kind, group)
	if !ok {
		return collectorTarget{resource: kind, group: group, action: "list/watch"}
	}
	action := "list/watch"
	if directRead(gvr, verb) {
		action = verb
	}
	return collectorTarget{resource: gvr.Resource, group: gvr.Group, action: action}
}

// directRead reports a read that goes straight to the API server instead of an
// informer.
func directRead(gvr schema.GroupVersionResource, verb string) bool {
	return k8s.ShouldBypassDynamicInformer(gvr) ||
		(verb == "get" && ((gvr.Group == "apiextensions.k8s.io" && gvr.Resource == "customresourcedefinitions") ||
			(gvr.Group == "apiregistration.k8s.io" && gvr.Resource == "apiservices")))
}

// collectorRemedy says how Radar's own collection of resource is widened, in
// this deployment's terms. With auth on the caller is not Radar's identity, so
// it also says their own access is not the fix; with auth off the caller runs
// Radar with that identity and that sentence would be wrong.
func collectorRemedy(ctx context.Context, resource string) string {
	var fix string
	if k8s.IsInCluster() {
		fix = fmt.Sprintf("grant Radar's service account list/watch on %s there", resource)
	} else {
		fix = fmt.Sprintf("Radar collects %s only in namespaces its kubeconfig identity can list, at most --max-scope-candidates of them (default 20); name more with --namespaces", resource)
	}
	if pkgauth.UserFromContext(ctx) != nil {
		return "; granting your role access does not change this; " + fix
	}
	return "; " + fix
}

// coverageError reports requested namespaces Radar's collector could not
// serve. With a refusal — Radar read them for this request and the API server
// denied its identity — the code is collector_forbidden; without one they lie
// outside the coverage the collector established, Radar did not read them,
// and the code is collector_scope_limited. No uncovered namespaces means the
// collector covers only some namespaces of an all-namespaces read. Only
// namespaces the caller may read are named, and coverage in namespaces the
// caller cannot read never changes the wording, so the error discloses
// nothing about other tenants' namespaces.
func coverageError(ctx context.Context, target collectorTarget, uncovered, covered []string, refusal error) error {
	covered = slices.DeleteFunc(slices.Clone(covered), func(ns string) bool { return slices.Contains(uncovered, ns) })
	covered = callerVisibleNamespaces(ctx, covered)
	where := "at cluster scope"
	if len(uncovered) > 0 {
		uncovered = callerVisibleNamespaces(ctx, uncovered)
		switch len(uncovered) {
		case 0:
			where = "in the requested namespaces"
		case 1:
			where = fmt.Sprintf("in namespace %q", uncovered[0])
		default:
			where = fmt.Sprintf("in namespaces %q", namespaceForError(uncovered))
		}
	}
	var coverage string
	switch {
	case len(covered) > 0:
		coverage = fmt.Sprintf("; covered namespaces you can read: %q", namespaceForError(covered))
	case refusal == nil:
		// A refusal for this request says nothing about where else the
		// collector could read, so only established coverage gets this.
		coverage = "; the collector's coverage does not include any namespace you can read"
	}
	remedy := collectorRemedy(ctx, target.resource)
	if refusal != nil {
		return &readError{code: readreason.CollectorForbidden, err: &collectorError{
			msg:   fmt.Sprintf("%s can't %s %s in API group %q %s%s%s", collectorName(), target.action, target.resource, target.group, where, coverage, remedy),
			cause: refusal,
		}}
	}
	if len(uncovered) == 0 && where == "at cluster scope" {
		return readErrorf(readreason.CollectorScopeLimited, "Radar collects %s only in some namespaces%s%s", target.resource, coverage, remedy)
	}
	verb := "is"
	if len(uncovered) != 1 {
		verb = "are"
	}
	return readErrorf(readreason.CollectorScopeLimited, "Radar does not watch %s %s, which %s outside the collector's scope%s%s", target.resource, where, verb, coverage, remedy)
}

// collectorError is a caller-facing error about a read Radar made with its own
// identity. Its message never includes the API server's response, which names
// that identity (for a service account, the namespace Radar runs in) and the
// scope it was refused in. The response stays reachable through Unwrap, for
// classification and for collectorCause to log.
type collectorError struct {
	msg   string
	cause error
}

func (e *collectorError) Error() string { return e.msg }
func (e *collectorError) Unwrap() error { return e.cause }

// collectorCause returns the API server response behind a collector error, or
// nil when err carries none.
func collectorCause(err error) error {
	var cause error
	for {
		var ce *collectorError
		if !errors.As(err, &ce) || ce.cause == nil {
			return cause
		}
		cause, err = ce.cause, ce.cause
	}
}

func collectorName() string {
	if k8s.IsInCluster() {
		return "Radar's service account"
	}
	return "Radar's kubeconfig identity"
}

// collectorAuthError is the caller-facing error for the API server refusing a
// read Radar made with its own identity, or nil when err is not a refusal.
func collectorAuthError(ctx context.Context, err error, target collectorTarget, namespace string) error {
	switch {
	case apierrors.IsUnauthorized(err):
		return collectorUnauthorizedError(target.resource, err)
	case apierrors.IsForbidden(err):
		var uncovered []string
		if namespace != "" {
			uncovered = []string{namespace}
		}
		return coverageError(ctx, target, uncovered, nil, err)
	}
	return nil
}

// callerVisibleNamespaces keeps the namespaces the caller may read. It fails
// closed: when the caller's access cannot be resolved, nothing is kept.
func callerVisibleNamespaces(ctx context.Context, namespaces []string) []string {
	visible, err := resourceReadNamespaces(ctx, nil)
	if err != nil {
		return nil
	}
	if visible == nil {
		return namespaces
	}
	var out []string
	for _, ns := range namespaces {
		if slices.Contains(visible, ns) {
			out = append(out, ns)
		}
	}
	return out
}

// Dynamic reads start informers on demand. Check their scope after the read so
// a cold or incomplete store cannot establish absence. The bool reports that
// the scope is now synced, so a read that may have preceded the sync can be
// repeated. Direct API reads need no informer and remain authoritative without
// one.
func checkDynamicResourceRead(ctx context.Context, kind, group, namespace, verb string, readErr error) (bool, error) {
	dynamicCache := k8s.GetDynamicResourceCache()
	gvr, ok := dynamicReadGVR(kind, group)
	return checkDynamicResourceReadFrom(ctx, kind, group, namespace, verb, readErr, dynamicCache, gvr, ok)
}

func checkDynamicResourceReadFrom(ctx context.Context, kind, group, namespace, verb string, readErr error, dynamicCache *k8s.DynamicResourceCache, gvr schema.GroupVersionResource, ok bool) (bool, error) {
	var classified *readError
	if errors.As(readErr, &classified) {
		return false, readErr
	}
	if errors.Is(readErr, k8s.ErrUnknownDynamicKind) {
		return false, readErrorf(readreason.KindUnknown, "%w", readErr)
	}
	if errors.Is(readErr, k8s.ErrDynamicNotReady) {
		if k8s.GetConnectionStatus().State == k8s.StateConnecting {
			return false, readErrorf(readreason.KindSyncPending, "%s: %w; cluster connection is still loading, please retry shortly", kind, readErr)
		}
		if ok && dynamicCache != nil {
			if observation := dynamicCache.Observation(gvr); observation.State == k8score.DynamicObservationDeferred && observation.ReasonCode == "watch_start_failed" {
				return false, readErrorf(readreason.KindWatchFailed, "Radar's watch for %s failed to start; retrying this read starts it again", kind)
			}
		}
		return false, readErrorf(readreason.KindNotWatched, "%s: %w", kind, readErr)
	}
	direct := ok && directRead(gvr, verb)
	if apierrors.IsForbidden(readErr) && ok && dynamicCache != nil && !direct && namespace != "" {
		var covered []string
		if observation := dynamicCache.Observation(gvr); observation.Scope == k8score.DynamicObservationScopeExplicitNamespaces {
			covered = observation.Namespaces
		}
		return false, coverageError(ctx, dynamicCollectorTarget(kind, group, verb), []string{namespace}, covered, readErr)
	}
	if err := collectorAuthError(ctx, readErr, dynamicCollectorTarget(kind, group, verb), namespace); err != nil {
		return false, err
	}
	if readErr != nil && !isObjectNotFound(readErr) {
		// A kind discovery lists without watch fails the read before any
		// informer exists; the observation says so without parsing the error.
		if ok && dynamicCache != nil && !direct && dynamicCache.Observation(gvr).State == k8score.DynamicObservationUnsupported {
			return false, readErrorf(readreason.KindNotWatchable, "%s does not support list/watch, so Radar cannot collect it", kind)
		}
		return false, readErrorf(readreason.ReadError, "failed to %s %s: %w", verb, kind, readErr)
	}
	if !ok || dynamicCache == nil || direct {
		return false, readErr
	}
	if readErr == nil || isObjectNotFound(readErr) {
		if !dynamicCache.IsNamespaceSynced(gvr, namespace) {
			_, waitErr := dynamicCache.ListBlocking(gvr, namespace, dynamicSyncWait)
			if waitErr != nil {
				return checkDynamicResourceReadFrom(ctx, kind, group, namespace, verb, waitErr, dynamicCache, gvr, ok)
			}
		}
	}
	observation := dynamicCache.Observation(gvr)
	switch observation.State {
	case k8score.DynamicObservationSyncing:
		if !dynamicCache.IsNamespaceSynced(gvr, namespace) {
			// Stalled is this namespace's informer, not the oldest one any
			// request started for the kind.
			if dynamicCache.NamespaceObservation(gvr, namespace).ReasonCode == "sync_stalled" {
				return false, readErrorf(readreason.KindSyncFailed, "%s have not finished loading; Radar keeps trying and they may still finish, so retry later, and if it persists check Radar's collector connection and list/watch permissions", kind)
			}
			return false, readErrorf(readreason.KindSyncPending, "%s are still loading, please retry shortly", kind)
		}
	case k8score.DynamicObservationSynced:
		if namespace != "" && !dynamicCache.IsNamespaceSynced(gvr, namespace) {
			return false, readErrorf(readreason.CollectorScopeLimited, "Radar does not watch %s in namespace %q, which is outside the collector's scope", kind, namespace)
		}
	case k8score.DynamicObservationUnsupported:
		return false, readErrorf(readreason.KindNotWatchable, "%s does not support list/watch, so Radar cannot collect it", kind)
	}
	return dynamicCache.IsNamespaceSynced(gvr, namespace), readErr
}

// dynamicSyncWait bounds a read's wait for a cold informer. A variable so tests
// can wait less than the production three seconds.
var dynamicSyncWait = 3 * time.Second

func discoveredGVR(kind, group string) (schema.GroupVersionResource, bool) {
	discovery := k8s.GetResourceDiscovery()
	if discovery == nil {
		return schema.GroupVersionResource{}, false
	}
	if group != "" {
		return discovery.GetGVRWithGroup(kind, group)
	}
	return discovery.GetGVR(kind)
}

func dynamicReadGVR(kind, group string) (schema.GroupVersionResource, bool) {
	if gvr, ok := discoveredGVR(kind, group); ok {
		return gvr, true
	}
	gvr, ok := k8s.BuiltinGVRAnyGroup(kind)
	return gvr, ok && (group == "" || group == gvr.Group)
}

// readDynamicScope performs one dynamic-cache read and checks its scope. A
// read that may have started before the scope's informer finished its initial
// sync can have served a partial store, so it is repeated once sync is proven.
// A scope already synced before and after the read was answered by that read;
// repeating it would only deep-copy the store a second time.
func readDynamicScope[T any](ctx context.Context, kind, group, namespace, verb string, read func() (T, error)) (T, error) {
	dynamicCache := k8s.GetDynamicResourceCache()
	discovery := k8s.GetResourceDiscovery()
	cache := k8s.GetResourceCache()
	gvr, ok := dynamicReadGVR(kind, group)
	current := func() bool {
		return dynamicCache == k8s.GetDynamicResourceCache() && discovery == k8s.GetResourceDiscovery() && cache == k8s.GetResourceCache()
	}
	synced := func() bool { return ok && dynamicCache != nil && dynamicCache.IsNamespaceSynced(gvr, namespace) }
	syncedBefore := synced()
	out, err := read()
	if !current() {
		return out, errConnectionChanged()
	}
	settled := syncedBefore && synced()
	ready, err := checkDynamicResourceReadFrom(ctx, kind, group, namespace, verb, err, dynamicCache, gvr, ok)
	if ready && !settled && (err == nil || isObjectNotFound(err)) {
		out, err = read()
		if err != nil {
			_, err = checkDynamicResourceReadFrom(ctx, kind, group, namespace, verb, err, dynamicCache, gvr, ok)
		}
	}
	if !current() {
		return out, errConnectionChanged()
	}
	return out, err
}

func isObjectNotFound(err error) bool {
	return apierrors.IsNotFound(err) || errors.Is(err, k8score.ErrResourceNotFound)
}

// narrowSecrets keeps the namespaces of target where the caller can list (or
// get) Secrets. Namespace access does not imply Secret access, so withheld
// namespaces are a gap within the target, not a narrowing of it. An
// unrestricted caller is checked cluster-wide.
func narrowSecrets(ctx context.Context, target []string, verb string) ([]string, gapSet, error) {
	var gaps gapSet
	if target == nil {
		granted, checked := canReadInNamespaceDecision(ctx, "", "secrets", "", verb)
		if !granted {
			return nil, gaps, resourcePermissionError(checked, verb, "", "secrets", "")
		}
		return nil, gaps, nil
	}
	namespaces, checked := filterNamespacesByCanReadDecision(ctx, "", "secrets", verb, target)
	if !checked {
		return nil, gaps, resourcePermissionError(false, verb, "", "secrets", namespaceForError(target))
	}
	if len(namespaces) == 0 {
		return nil, gaps, resourcePermissionError(true, verb, "", "secrets", namespaceForError(target))
	}
	if withheld := slices.DeleteFunc(slices.Clone(target), func(ns string) bool { return slices.Contains(namespaces, ns) }); len(withheld) > 0 {
		gaps.add(readreason.RBACDenied, resourcePermissionError(true, verb, "", "secrets", namespaceForError(withheld)), withheld...)
	}
	return namespaces, gaps, nil
}

func resourceKindName(kind, group string) string {
	if gvr, ok := k8s.BuiltinGVRAnyGroup(kind); ok && (group == "" || group == gvr.Group) {
		return gvr.Resource
	}
	return kind
}

// resourceGetError reports a get's failure. A miss is an answer only because
// callers reach this after establishing that the namespace is readable.
func resourceGetError(ctx context.Context, err error, kind, namespace, name string) error {
	if isObjectNotFound(err) {
		return &readError{code: readreason.ObjectNotFound, err: notFoundError(ctx, err, kind, namespace, name)}
	}
	return readErrorf(readreason.ReadError, "failed to get %s %s/%s: %w", kind, namespace, name, err)
}

func fetchMCPDynamicResource(ctx context.Context, cache *k8s.ResourceCache, kind, group, namespace, name string) (*unstructured.Unstructured, error) {
	dynamicCache := k8s.GetDynamicResourceCache()
	discovery := k8s.GetResourceDiscovery()
	gvr, known := dynamicReadGVR(kind, group)
	current := func() bool {
		return cache == k8s.GetResourceCache() && dynamicCache == k8s.GetDynamicResourceCache() && discovery == k8s.GetResourceDiscovery()
	}
	if !current() {
		return nil, errConnectionChanged()
	}
	obj, err := readDynamicScope(ctx, kind, group, namespace, "get", func() (*unstructured.Unstructured, error) {
		if !current() {
			return nil, errConnectionChanged()
		}
		if known && dynamicCache != nil && !k8s.TypedKindOwnsGroup(kind, group) {
			if directRead(gvr, "get") {
				return dynamicCache.GetDirect(ctx, gvr, namespace, name)
			}
			return dynamicCache.Get(gvr, namespace, name)
		}
		return cache.GetDynamicWithGroup(ctx, kind, namespace, name, group)
	})
	if !current() {
		return nil, errConnectionChanged()
	}
	if isObjectNotFound(err) {
		return obj, resourceGetError(ctx, err, kind, namespace, name)
	}
	return obj, err
}

func collectorUnauthorizedError(kind string, err error) error {
	credentials := "kubeconfig credentials"
	if k8s.IsInCluster() {
		credentials = "service-account credentials"
	}
	return &readError{code: readreason.CollectorCredentialsRejected, err: &collectorError{
		msg:   fmt.Sprintf("Radar's %s were rejected while reading %s; refresh them and retry", credentials, kind),
		cause: err,
	}}
}
