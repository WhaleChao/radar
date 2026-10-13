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

// readError is a resource-read failure with the reason an agent branches on.
// Its text is "<reason>: <explanation>"; readErrorResult also sends the pair
// as the tool result's structuredContent.
type readError struct {
	reason readreason.Code
	err    error
}

func (e *readError) Error() string { return string(e.reason) + ": " + e.err.Error() }
func (e *readError) Unwrap() error { return e.err }

func readErrorf(reason readreason.Code, format string, args ...any) error {
	return &readError{reason: reason, err: fmt.Errorf(format, args...)}
}

// readErrorContent is the structuredContent of a resource-read tool error.
type readErrorContent struct {
	Reason readreason.Code `json:"reason"`
	Error  string          `json:"error"`
}

// readErrorResult renders a readError as an MCP tool error whose
// structuredContent carries the reason, so clients need not parse the text.
// It reports false for any other error, which keeps the SDK's text-only form.
func readErrorResult(err error) (*mcp.CallToolResult, bool) {
	var re *readError
	if !errors.As(err, &re) {
		return nil, false
	}
	result := &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: err.Error()}},
		StructuredContent: readErrorContent{Reason: re.reason, Error: re.err.Error()},
	}
	result.SetError(err)
	return result, true
}

// listCoverage says a list_resources read did not cover the cluster: Radar's
// collector could read the kind only in a finite namespace set, so rows cannot
// establish that an object is absent anywhere else. A complete read carries
// none and stays a bare array.
type listCoverage struct {
	resourceapi.Coverage
	Note string `json:"note"`
}

// partialList is a partial read's result, sent as the result's only text
// block. Findings parses the text agent backends hand it, and the evidence
// ledger matches that text against the producer's exactly, so the rows and
// their coverage must be one JSON value: a second block would be joined into
// something neither accepts. Coverage leads so a reader that keeps only the
// head of a long result still has the caveat.
type partialList struct {
	Coverage listCoverage `json:"coverage"`
	Items    []any        `json:"items"`
}

func collectorLimitedCoverage(resource string, reason readreason.Code, read []string) *listCoverage {
	read = slices.Clone(read)
	slices.Sort(read)
	where := fmt.Sprintf("namespace %q", read[0])
	if len(read) > 1 {
		where = fmt.Sprintf("namespaces %q", namespaceForError(read))
	}
	return &listCoverage{
		Coverage: resourceapi.Coverage{
			Status:    resourceapi.CoveragePartial,
			Requested: resourceapi.Scope{Type: resourceapi.ScopeAllNamespaces},
			Read:      resourceapi.Scope{Type: resourceapi.ScopeNamespaces, Namespaces: resourceapi.NamespacePreview(read, resourceapi.MCPNamespacePreview)},
			Gaps:      []resourceapi.Gap{{Reason: reason}},
		},
		Note: fmt.Sprintf("Radar read %s only in %s; objects in other namespaces were not read, so this result cannot show they don't exist.", resource, where),
	}
}

// listResult renders list_resources rows: a bare array for a complete read,
// a partialList for a partial one.
func listResult(items []any, coverage *listCoverage) (*mcp.CallToolResult, any, error) {
	if coverage == nil {
		return toJSONResult(items)
	}
	body, err := json.Marshal(partialList{Coverage: *coverage, Items: items})
	if err != nil {
		log.Printf("[mcp] Failed to marshal result: %v", err)
		return nil, nil, fmt.Errorf("failed to marshal result: %w", err)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(body)}}}, nil, nil
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

func resourceReadNamespaces(ctx context.Context, requested []string) ([]string, error) {
	clamped, ok := clampToNamespacePin(requested)
	if !ok {
		return nil, readErrorf(readreason.OutsideNamespaceScope, "requested namespace %q is excluded by Radar's --namespace-scope selection", namespaceForError(requested))
	}
	user, perms, checked := resolveUserPermsDecision(ctx)
	if !checked {
		return nil, readErrorf(readreason.PermissionCheckFailed, "could not discover your Radar namespace access; retry when the permission check is available")
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
		return nil, readErrorf(readreason.NoNamespaceAccess, "no Radar access to %s (Radar grants namespace access when your role can list pods or deployments there; re-checked every ~2 minutes)", scope)
	}
	return allowed, nil
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

func resourceReadCacheIsCurrent(cache *k8s.ResourceCache) bool {
	active, syncing := k8s.SnapshotCaches()
	if active != nil {
		return cache == active
	}
	return cache != nil && cache == syncing
}

// resourceReadCache returns the cache to read kind from and the namespaces to
// read under namespaceCoverageError's rule: nil (an unrestricted caller) reads
// the typed collector's covered namespaces, so a non-nil answer to nil means
// the read is partial, and a finite list must be covered in full. Dynamic kinds
// learn their coverage by reading, so their namespaces pass through.
func resourceReadCache(ctx context.Context, kind, group string, namespaces []string) (*k8s.ResourceCache, []string, error) {
	gvr, builtin := k8s.BuiltinGVRAnyGroup(kind)
	typed := builtin && (group == "" || group == gvr.Group) && k8s.TypedKindOwnsGroup(kind, gvr.Group)
	if !typed {
		cache := k8s.GetResourceCache()
		if cache == nil {
			return nil, nil, errNotConnected()
		}

		return cache, namespaces, nil
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
	assessment := cache.AssessKindRead(gvr.Resource, namespaces)
	switch assessment.Readiness {
	case k8s.KindPending:
		return nil, nil, readErrorf(readreason.KindSyncPending, "%s are still loading, please retry shortly", gvr.Resource)
	case k8s.KindFailed:
		return nil, nil, readErrorf(readreason.KindSyncFailed, "%s failed to load within the sync deadline; check Radar's collector connection and list/watch permissions", gvr.Resource)
	case k8s.KindUnavailable:
		if result != nil {
			if scope, probed := result.Scopes[gvr.Resource]; probed && !scope.Enabled {
				if apierrors.IsUnauthorized(result.ProbeErrors[gvr.Resource]) {
					return nil, nil, collectorUnauthorizedError(gvr.Resource, result.ProbeErrors[gvr.Resource])
				}
				return nil, nil, readErrorf(readreason.CollectorAccessDenied, "%s can't list/watch %s in API group %q", collectorName(), gvr.Resource, gvr.Group)
			}
		}
		return nil, nil, readErrorf(readreason.KindNotWatched, "Radar is not watching resource %q in API group %q; check Radar's collector list/watch permissions and collection configuration", gvr.Resource, gvr.Group)
	}
	covered := assessment.Read
	if assessment.AllNamespaces {
		return cache, namespaces, nil
	}
	if namespaces == nil {
		return cache, covered, nil
	}
	uncovered := assessment.Uncovered
	if err := namespaceCoverageError(ctx, collectorTarget{resource: gvr.Resource, group: gvr.Group}, false, 0, uncovered, cache.KindNamespaces(gvr.Resource), nil); err != nil {
		return nil, nil, err
	}
	return cache, namespaces, nil
}

// namespaceCoverageError is the one coverage rule typed, dynamic and direct
// reads share. A restricted caller (a finite namespace list — Radar's
// namespace access for the caller, an explicit namespace, or the
// --namespace-scope pin) needs every requested namespace covered: a partial
// answer would read as the whole of its scope. An unrestricted caller's
// unscoped read asks for whatever the collector covers (unrestrictedAll), so a
// namespace missing from that is omitted, the result names the namespaces read
// (listCoverage), and only a read that covered nothing fails. refusal is the
// API server's denial of the first uncovered namespace when Radar read them
// for this request; nil means they lie outside the coverage the collector
// established.
func namespaceCoverageError(ctx context.Context, target collectorTarget, unrestrictedAll bool, read int, uncovered, covered []string, refusal error) error {
	if len(uncovered) == 0 || (unrestrictedAll && read > 0) {
		return nil
	}
	return coverageError(ctx, target, uncovered, covered, refusal)
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

// coverageError reports requested namespaces Radar's collector could not
// serve. With a refusal — Radar read them for this request and the API server
// denied its identity — the reason is collector_access_denied; without one
// they lie outside the coverage the collector established, Radar did not read
// them, and the reason is collector_scope_limited. No uncovered namespaces
// means the cluster scope. Only namespaces the caller may read are named, and
// coverage in namespaces the caller cannot read never changes the wording, so
// the error discloses nothing about other tenants' namespaces.
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
		coverage = fmt.Sprintf("; covered namespaces you can read: %q; retry with one of them as the namespace", namespaceForError(covered))
	case refusal == nil:
		// A refusal for this request says nothing about where else the
		// collector could read, so only established coverage gets this.
		coverage = "; the collector's coverage does not include any namespace you can read"
	}
	if refusal != nil {
		return &readError{reason: readreason.CollectorAccessDenied, err: &collectorError{
			msg:   fmt.Sprintf("%s can't %s %s in API group %q %s; granting your role access does not change this%s", collectorName(), target.action, target.resource, target.group, where, coverage),
			cause: refusal,
		}}
	}
	verb := "is"
	if len(uncovered) != 1 {
		verb = "are"
	}
	return readErrorf(readreason.CollectorScopeLimited, "Radar does not watch %s %s, which %s outside the collector's scope%s", target.resource, where, verb, coverage)
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
		return false, readErrorf(readreason.UnknownKind, "%w", readErr)
	}
	if errors.Is(readErr, k8s.ErrDynamicNotReady) {
		if k8s.GetConnectionStatus().State == k8s.StateConnecting {
			return false, readErrorf(readreason.KindSyncPending, "%s: %w; cluster connection is still loading, please retry shortly", kind, readErr)
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
			return false, readErrorf(readreason.CollectionNotSupported, "%s does not support list/watch, so Radar cannot collect it", kind)
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
				return false, readErrorf(readreason.KindSyncFailed, "%s initial cache sync has stalled; check Radar's collector connection and list/watch permissions", kind)
			}
			return false, readErrorf(readreason.KindSyncPending, "%s are still loading, please retry shortly", kind)
		}
	case k8score.DynamicObservationSynced:
		if namespace != "" && !dynamicCache.IsNamespaceSynced(gvr, namespace) {
			return false, readErrorf(readreason.CollectorScopeLimited, "Radar does not watch %s in namespace %q, which is outside the collector's scope", kind, namespace)
		}
	case k8score.DynamicObservationUnsupported:
		return false, readErrorf(readreason.CollectionNotSupported, "%s does not support list/watch, so Radar cannot collect it", kind)
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
		return out, readErrorf(readreason.KindSyncPending, "cluster context changed during the read; retry shortly")
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
		return out, readErrorf(readreason.KindSyncPending, "cluster context changed during the read; retry shortly")
	}
	return out, err
}

func isObjectNotFound(err error) bool {
	return apierrors.IsNotFound(err) || errors.Is(err, k8score.ErrResourceNotFound)
}

func resourceSecretNamespaces(ctx context.Context, allowed []string, verb string) ([]string, error) {
	if allowed == nil {
		granted, checked := canReadInNamespaceDecision(ctx, "", "secrets", "", verb)
		if !granted {
			return nil, resourcePermissionError(checked, verb, "", "secrets", "")
		}
		return nil, nil
	}
	namespaces, checked := filterNamespacesByCanReadDecision(ctx, "", "secrets", verb, allowed)
	if !checked {
		return nil, resourcePermissionError(false, verb, "", "secrets", namespaceForError(allowed))
	}
	if len(namespaces) > 0 {
		return namespaces, nil
	}
	return nil, resourcePermissionError(true, verb, "", "secrets", namespaceForError(allowed))
}

func resourceKindName(kind, group string) string {
	if gvr, ok := k8s.BuiltinGVRAnyGroup(kind); ok && (group == "" || group == gvr.Group) {
		return gvr.Resource
	}
	return kind
}

func resourceGetError(ctx context.Context, err error, kind, namespace, name string) error {
	if isObjectNotFound(err) {
		return notFoundError(ctx, err, kind, namespace, name)
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
		return nil, readErrorf(readreason.KindSyncPending, "cluster context changed during the read; retry shortly")
	}
	obj, err := readDynamicScope(ctx, kind, group, namespace, "get", func() (*unstructured.Unstructured, error) {
		if !current() {
			return nil, readErrorf(readreason.KindSyncPending, "cluster context changed during the read; retry shortly")
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
		return nil, readErrorf(readreason.KindSyncPending, "cluster context changed during the read; retry shortly")
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
	return &readError{reason: readreason.CollectorUnauthorized, err: &collectorError{
		msg:   fmt.Sprintf("Radar's %s were rejected while reading %s; refresh them and retry", credentials, kind),
		cause: err,
	}}
}
