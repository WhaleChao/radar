package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/skyhook-io/radar/internal/k8s"
	"github.com/skyhook-io/radar/pkg/k8score"
	"github.com/skyhook-io/radar/pkg/readreason"
	"github.com/skyhook-io/radar/pkg/resourceapi"
	"golang.org/x/sync/errgroup"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const resourceListHardLimit = 50000

// readStatus is the HTTP status of a failed resource read, by code. It is
// transport only — clients branch on error_code — but it is chosen so a UI or
// fleet client that branches on status alone still does the right thing:
// Radar's own collection is never 401/403 (that reads as "you lack access"),
// and a configuration limit is never 503 (that reads as "retry"). Fleet
// clients exclude 503 from their cluster circuit breaker, so a collector
// failure scoped to one kind is 503 rather than another 5xx.
var readStatus = map[readreason.Code]int{
	readreason.RBACDenied:                   http.StatusForbidden,
	readreason.NoNamespaceAccess:            http.StatusForbidden,
	readreason.NamespaceNotVisible:          http.StatusForbidden,
	readreason.PermissionCheckFailed:        http.StatusServiceUnavailable,
	readreason.CollectorForbidden:           http.StatusServiceUnavailable,
	readreason.CollectorCredentialsRejected: http.StatusServiceUnavailable,
	readreason.CollectorScopeLimited:        http.StatusConflict,
	readreason.KindSyncPending:              http.StatusServiceUnavailable,
	readreason.KindSyncFailed:               http.StatusServiceUnavailable,
	readreason.KindWatchFailed:              http.StatusServiceUnavailable,
	readreason.KindNotWatched:               http.StatusServiceUnavailable,
	readreason.KindUnknown:                  http.StatusBadRequest,
	readreason.KindNotWatchable:             http.StatusBadRequest,
	readreason.OutsideNamespaceScope:        http.StatusConflict,
	readreason.OutsideNamespaceSelection:    http.StatusConflict,
	readreason.NamespaceNotFound:            http.StatusNotFound,
	readreason.NamespaceRequired:            http.StatusBadRequest,
	readreason.ObjectNotFound:               http.StatusNotFound,
	readreason.ListTooLarge:                 http.StatusBadRequest,
	readreason.FilterEvalFailed:             http.StatusBadRequest,
	readreason.ClusterConnecting:            http.StatusServiceUnavailable,
	readreason.ClusterDisconnected:          http.StatusServiceUnavailable,
	readreason.ConnectionChanged:            http.StatusConflict,
	readreason.ReadError:                    http.StatusInternalServerError,
}

func readStatusFor(code readreason.Code) int {
	if status, ok := readStatus[code]; ok {
		return status
	}
	return http.StatusInternalServerError
}

// resourceReadFailure is a read that served nothing. gaps keeps every reason
// when namespaces failed for different ones; code is the most actionable.
type resourceReadFailure struct {
	code    readreason.Code
	message string
	gaps    []resourceapi.Gap
}

func (e *resourceReadFailure) Error() string { return e.message }

func readFailuref(code readreason.Code, format string, args ...any) *resourceReadFailure {
	return &resourceReadFailure{code: code, message: fmt.Sprintf(format, args...)}
}

// errReadHandlesChanged marks a read whose cache, dynamic cache or discovery
// was replaced while it ran, so its rows may mix two clusters.
var errReadHandlesChanged = errors.New("Radar's cluster connection changed during this read; confirm which cluster Radar is connected to, then retry")

func collectionFailure(resource string, err error) *resourceReadFailure {
	switch {
	case errors.Is(err, errReadHandlesChanged):
		return readFailuref(readreason.ConnectionChanged, "%s", err.Error())
	case errors.Is(err, k8score.ErrResourceListTooLarge):
		return readFailuref(readreason.ListTooLarge, "Too many resources to load; select fewer namespaces")
	case errors.Is(err, k8score.ErrCollectionNotSupported):
		return readFailuref(readreason.KindNotWatchable, "%s cannot be collected with list/watch", resource)
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		return readFailuref(readreason.ReadError, "Kubernetes read did not finish within the request budget; retry with fewer namespaces")
	case apierrors.IsUnauthorized(err):
		return readFailuref(readreason.CollectorCredentialsRejected, "Kubernetes rejected Radar's collector credentials")
	case apierrors.IsForbidden(err):
		return readFailuref(readreason.CollectorForbidden, "Radar's collector cannot read %s in the requested scope", resource)
	case errors.Is(err, k8s.ErrUnknownDynamicKind):
		return readFailuref(readreason.KindUnknown, "%s", err.Error())
	case errors.Is(err, k8s.ErrDynamicNotReady):
		return readFailuref(readreason.KindSyncPending, "Resource discovery is still loading")
	default:
		return readFailuref(readreason.ReadError, "Radar could not read %s", resource)
	}
}

// writeReadFailure answers a failed read with {error, error_code, coverage?}.
// target, when the read has one, gives the failure its coverage: the scope
// and every gap, with no read scope because Radar read nothing.
func (s *Server) writeReadFailure(w http.ResponseWriter, resource string, err error, target *listTarget) {
	var failure *resourceReadFailure
	if !errors.As(err, &failure) {
		failure = collectionFailure(resource, err)
	}
	if failure.code == readreason.ReadError {
		log.Printf("[resource-read] Failed to read %s: %v", resource, err)
	}
	body := resourceapi.ReadError{Error: failure.message, ErrorCode: failure.code}
	if target != nil && (len(failure.gaps) > 0 || len(target.gaps) > 0) {
		gaps := append(slices.Clone(target.gaps), failure.gaps...)
		if len(gaps) == 0 {
			gaps = []resourceapi.Gap{{Code: failure.code}}
		}
		body.Coverage = &resourceapi.Coverage{Status: resourceapi.CoveragePartial, Scope: target.scope(), NarrowedBy: target.narrowedBy, Gaps: gaps}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(readStatusFor(failure.code))
	s.writeJSON(w, body)
}

// writeCollectionFailure answers a failed read that has no list target.
func (s *Server) writeCollectionFailure(w http.ResponseWriter, resource string, err error) {
	s.writeReadFailure(w, resource, err, nil)
}

// listTarget is what a list is about: the namespaces the request named, or
// what remained of every namespace after the --namespace-scope pin, the saved
// namespace pick and the caller's access narrowed it. A named namespace stays
// in scope even when it can't be read, so what's missing is a gap; implicit
// narrowing is declared instead. gaps are the caller's own: named namespaces
// they can't see, and namespaces they can see where exact kind authorization
// denies the kind.
type listTarget struct {
	clusterScoped bool
	namespaces    []string
	narrowedBy    []resourceapi.Narrowing
	gaps          []resourceapi.Gap
}

func (t listTarget) scope() resourceapi.Scope {
	switch {
	case t.clusterScoped:
		return resourceapi.Scope{Type: resourceapi.ScopeCluster}
	case t.namespaces == nil:
		return resourceapi.Scope{Type: resourceapi.ScopeAllNamespaces}
	default:
		return resourceapi.Scope{Type: resourceapi.ScopeNamespaces, Namespaces: resourceapi.Names(t.namespaces, resourceapi.RESTSample)}
	}
}

// coverage is a successful list's coverage: read is the namespaces read, or
// nil when Radar read the whole scope.
func (t listTarget) coverage(read []string, gaps []resourceapi.Gap) resourceapi.Coverage {
	gaps = append(slices.Clone(t.gaps), gaps...)
	cov := resourceapi.Coverage{Status: resourceapi.CoverageComplete, Scope: t.scope(), NarrowedBy: t.narrowedBy, Gaps: gaps}
	readScope := cov.Scope
	if read != nil && !t.clusterScoped {
		readScope = resourceapi.Scope{Type: resourceapi.ScopeNamespaces, Namespaces: resourceapi.Names(read, resourceapi.RESTSample)}
	}
	cov.Read = &readScope
	if len(gaps) > 0 {
		cov.Status = resourceapi.CoveragePartial
	}
	if cov.Gaps == nil {
		cov.Gaps = []resourceapi.Gap{}
	}
	return cov
}

// resourceListTarget resolves a list's target from the request. visible is
// parseNamespacesForUser's answer (nil: every namespace) and authorized what
// exact kind authorization allows of it.
func (s *Server) resourceListTarget(r *http.Request, visible, authorized []string, clusterScoped bool) listTarget {
	if clusterScoped {
		return listTarget{clusterScoped: true}
	}
	return s.requestListTarget(r, visible).withKindAuthorization(visible, authorized)
}

// requestListTarget is the part of a list's target that depends only on the
// request, not on the kind.
func (s *Server) requestListTarget(r *http.Request, visible []string) listTarget {
	t := listTarget{namespaces: visible}
	if named := parseNamespaces(r.URL.Query()); named != nil {
		// A named namespace the caller can't see is a gap counted but never
		// named back: the caller knows what they asked for, and the response
		// stays the same whether that namespace exists or not.
		named = slices.Compact(slices.Sorted(slices.Values(named)))
		pinned, hidden := 0, 0
		t.namespaces = []string{}
		for _, ns := range named {
			switch {
			case visible == nil || slices.Contains(visible, ns):
				t.namespaces = append(t.namespaces, ns)
			case k8s.ForceNamespaceScope && ns != k8s.GetNamespaceScopeTarget():
				pinned++
			default:
				hidden++
			}
		}
		if pinned > 0 {
			t.gaps = append(t.gaps, resourceapi.Gap{Code: readreason.OutsideNamespaceScope, Namespaces: &resourceapi.NameSet{Count: pinned, Sample: []string{}}})
		}
		if hidden > 0 {
			t.gaps = append(t.gaps, resourceapi.Gap{Code: readreason.NamespaceNotVisible, Namespaces: &resourceapi.NameSet{Count: hidden, Sample: []string{}}})
		}
	} else if visible != nil {
		if s.getUserNamespaces(r, nil) != nil {
			t.narrowedBy = append(t.narrowedBy, resourceapi.NarrowedByCallerAccess)
		}
		if k8s.ForceNamespaceScope {
			t.narrowedBy = append(t.narrowedBy, resourceapi.NarrowedByNamespacePin)
		} else if _, picks := s.getActiveNamespaceForUserInContext(r); len(picks) > 0 {
			t.narrowedBy = append(t.narrowedBy, resourceapi.NarrowedByNamespaceSelection)
		}
	}
	return t
}

// withKindAuthorization adds the namespaces exact kind authorization (Secrets,
// Roles, RoleBindings) withholds from what the caller can otherwise see. They
// were visible to the caller already, so naming them leaks nothing.
func (t listTarget) withKindAuthorization(visible, authorized []string) listTarget {
	t.gaps = slices.Clone(t.gaps)
	if authorized != nil {
		if visible == nil {
			t.gaps = append(t.gaps, resourceapi.Gap{Code: readreason.RBACDenied})
		} else if withheld := slices.DeleteFunc(slices.Clone(visible), func(ns string) bool { return slices.Contains(authorized, ns) }); len(withheld) > 0 {
			t.gaps = append(t.gaps, resourceapi.Gap{Code: readreason.RBACDenied, Namespaces: resourceapi.Names(withheld, resourceapi.RESTSample)})
		}
	}
	return t
}

// typedRead is what a typed kind's cache can serve of a target now.
type typedRead struct {
	// read: the namespaces to read; nil means the whole target.
	read []string
	// whole: the kind is ready as a whole, so its listers serve the read; a
	// kind still loading elsewhere is read from its namespaces' own stores.
	whole bool
	gaps  []resourceapi.Gap
}

// typedReadCoverage assesses what of namespaces (nil: every namespace) a typed
// kind can be read in now, and why the rest can't. It fails only when nothing
// can be read.
func typedReadCoverage(cache *k8s.ResourceCache, resource string, namespaces []string, clusterScoped bool) (typedRead, error) {
	a := cache.AssessKindRead(resource, namespaces)
	pending := func(ns ...string) resourceapi.Gap {
		return gapOf(readreason.KindSyncPending, ns)
	}
	failed := func(ns ...string) resourceapi.Gap {
		return gapOf(readreason.KindSyncFailed, ns)
	}
	switch {
	case a.Readiness == k8s.KindUnavailable:
		if permissions := cache.PermissionResult(); permissions != nil {
			if scope, known := permissions.Scopes[resource]; known && !scope.Enabled {
				failure := collectionFailure(resource, permissions.ProbeErrors[resource])
				if failure.code == readreason.ReadError {
					failure = readFailuref(readreason.CollectorForbidden, "Radar's collector cannot list/watch %s", resource)
				}
				return typedRead{}, failure
			}
		}
		return typedRead{}, readFailuref(readreason.KindNotWatched, "Radar is not watching %s", resource)
	case a.AllNamespaces && namespaces == nil:
		switch a.Readiness {
		case k8s.KindReady:
			return typedRead{whole: true}, nil
		case k8s.KindFailed:
			return typedRead{}, &resourceReadFailure{code: readreason.KindSyncFailed, message: fmt.Sprintf("%s did not finish loading within the sync deadline; Radar keeps trying, so retry later", resource), gaps: []resourceapi.Gap{failed()}}
		default:
			return typedRead{}, &resourceReadFailure{code: readreason.KindSyncPending, message: fmt.Sprintf("%s are still loading, please retry shortly", resource), gaps: []resourceapi.Gap{pending()}}
		}
	}
	var gaps []resourceapi.Gap
	if len(a.Uncovered) > 0 {
		gaps = append(gaps, gapOf(readreason.CollectorScopeLimited, a.Uncovered))
	}
	if namespaces == nil && !a.AllNamespaces {
		gaps = append(gaps, resourceapi.Gap{Code: readreason.CollectorScopeLimited})
	}
	if len(a.Loading) > 0 {
		gaps = append(gaps, pending(a.Loading...))
	}
	if len(a.Failed) > 0 {
		gaps = append(gaps, failed(a.Failed...))
	}
	if len(a.Read) == 0 && !(namespaces != nil && len(namespaces) == 0) {
		codes := make([]readreason.Code, 0, len(gaps))
		for _, gap := range gaps {
			codes = append(codes, gap.Code)
		}
		code := readreason.Primary(codes)
		message := fmt.Sprintf("Radar's collector does not cover the requested scope for %s", resource)
		switch code {
		case readreason.KindSyncPending:
			message = fmt.Sprintf("%s are still loading, please retry shortly", resource)
		case readreason.KindSyncFailed:
			message = fmt.Sprintf("%s did not finish loading within the sync deadline; Radar keeps trying, so retry later", resource)
		}
		return typedRead{}, &resourceReadFailure{code: code, message: message, gaps: gaps}
	}
	read := a.Read
	if clusterScoped || (a.AllNamespaces && namespaces == nil) {
		read = nil
	}
	return typedRead{read: read, whole: a.Readiness == k8s.KindReady, gaps: gaps}, nil
}

func gapOf(code readreason.Code, namespaces []string) resourceapi.Gap {
	gap := resourceapi.Gap{Code: code}
	if len(namespaces) > 0 {
		gap.Namespaces = resourceapi.Names(namespaces, resourceapi.RESTSample)
	}
	return gap
}

func parseResourceListLimit(r *http.Request, summary bool) (int, error) {
	limit := 25000
	if summary {
		limit = resourceListHardLimit
	}
	if value := r.URL.Query().Get("maxItems"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > resourceListHardLimit {
			return 0, fmt.Errorf("maxItems must be between 1 and %d", resourceListHardLimit)
		}
		limit = parsed
	}
	return limit, nil
}

func (s *Server) checkResourceListSize(w http.ResponseWriter, count, limit int) bool {
	if count <= limit {
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	s.writeJSON(w, map[string]any{"error": "Too many resources to load; select fewer namespaces", "error_code": readreason.ListTooLarge, "count": count, "limit": limit})
	return false
}

func listSize(items any) int {
	if items == nil {
		return 0
	}
	value := reflect.ValueOf(items)
	if value.Kind() == reflect.Slice {
		return value.Len()
	}
	return 0
}

func typedListCount(cache *k8s.ResourceCache, resource string, namespaces []string) int {
	for _, kind := range k8score.AllKindListers() {
		if informerKeyForKind(kind.Kind()) == resource {
			return k8score.ListCountNamespaced(kind.Lister()(cache.ResourceCache), namespaces)
		}
	}
	return 0
}

// Rows and observations use these captured handles. A lifecycle change before
// capture completes is retryable rather than mixing data from two clusters.
func dynamicReadHandles(cache *k8s.ResourceCache, kind, group string) (*k8s.DynamicResourceCache, schema.GroupVersionResource, error) {
	dynamicCache := k8s.GetDynamicResourceCache()
	discovery := k8s.GetResourceDiscovery()
	if cache == nil || cache != k8s.GetResourceCache() || dynamicCache == nil || discovery == nil {
		return nil, schema.GroupVersionResource{}, k8s.ErrDynamicNotReady
	}
	var gvr schema.GroupVersionResource
	var found bool
	if group != "" {
		gvr, found = discovery.GetGVRWithGroup(kind, group)
	} else {
		gvr, found = discovery.GetGVR(kind)
	}
	if !found {
		gvr, found = k8s.BuiltinGVR(kind, group)
	}
	if !found {
		return nil, gvr, fmt.Errorf("%w: %s", k8s.ErrUnknownDynamicKind, kind)
	}
	if cache != k8s.GetResourceCache() || dynamicCache != k8s.GetDynamicResourceCache() || discovery != k8s.GetResourceDiscovery() {
		return nil, gvr, k8s.ErrDynamicNotReady
	}
	return dynamicCache, gvr, nil
}

func namespaceReadFailure(dynamicCache *k8s.DynamicResourceCache, gvr schema.GroupVersionResource, namespace string) *resourceReadFailure {
	observation := dynamicCache.NamespaceObservation(gvr, namespace)
	switch {
	case observation.ReasonCode == "sync_stalled":
		return readFailuref(readreason.KindSyncFailed, "%s have not finished loading; Radar keeps trying, so retry later", gvr.Resource)
	case observation.State == k8score.DynamicObservationDenied:
		return readFailuref(readreason.CollectorForbidden, "Radar's collector cannot read %s in the requested scope", gvr.Resource)
	case observation.State == k8score.DynamicObservationUnwatched && dynamicCache.Observation(gvr).State == k8score.DynamicObservationSynced:
		return readFailuref(readreason.CollectorScopeLimited, "Radar's collector does not cover the requested scope for %s", gvr.Resource)
	case observation.State == k8score.DynamicObservationUnsupported:
		return readFailuref(readreason.KindNotWatchable, "%s cannot be collected with list/watch", gvr.Resource)
	case observation.State == k8score.DynamicObservationDeferred && observation.ReasonCode == "watch_start_failed":
		return readFailuref(readreason.KindWatchFailed, "Radar's watch for %s failed to start; retrying this read starts it again", gvr.Resource)
	default:
		return readFailuref(readreason.KindSyncPending, "%s are still loading, please retry shortly", gvr.Resource)
	}
}

// readDynamicResourceList lists a dynamic kind in namespaces (nil: every
// namespace). Each namespace is a separate read: one that fails is a gap,
// and the list fails only when none succeeds — or when the list is over its
// size budget, or the read handles changed, which invalidate the whole answer.
// read is the namespaces read, nil when the whole target was.
func readDynamicResourceList(r *http.Request, cache *k8s.ResourceCache, kind, group string, namespaces []string, limit int) (rows []*unstructured.Unstructured, read []string, gaps []resourceapi.Gap, err error) {
	dynamicCache, gvr, err := dynamicReadHandles(cache, kind, group)
	if err != nil {
		return nil, nil, nil, err
	}
	clusterScoped, _, _ := k8s.ClassifyKindScope(kind, group)
	direct := k8s.ShouldBypassDynamicInformer(gvr)
	targets := namespaces
	residualUnknown := false
	if namespaces == nil {
		if direct {
			items, listErr := dynamicCache.ListDirectBounded(r.Context(), gvr, "", limit)
			if listErr == nil {
				return items, nil, nil, nil
			}
			if !apierrors.IsForbidden(listErr) || clusterScoped {
				return nil, nil, nil, listErr
			}
			targets = dynamicCache.FallbackNamespaces()
		} else {
			items, listErr := dynamicCache.ListContext(r.Context(), gvr, "")
			if listErr != nil {
				return nil, nil, nil, listErr
			}
			if dynamicCache.IsNamespaceSynced(gvr, "") {
				return items, nil, nil, nil
			}
			observation := dynamicCache.Observation(gvr)
			if observation.Scope != k8score.DynamicObservationScopeExplicitNamespaces {
				failure := namespaceReadFailure(dynamicCache, gvr, "")
				failure.gaps = []resourceapi.Gap{{Code: failure.code}}
				return nil, nil, nil, failure
			}
			targets = observation.Namespaces
		}
		residualUnknown = true
	}
	targets = slices.Compact(slices.Sorted(slices.Values(targets)))
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	var mu sync.Mutex
	read = []string{}
	failures := make(map[readreason.Code][]string)
	failureByCode := make(map[readreason.Code]*resourceReadFailure)
	tooLarge := false
	var groupReads errgroup.Group
	groupReads.SetLimit(8)
	for _, namespace := range targets {
		groupReads.Go(func() error {
			var items []*unstructured.Unstructured
			var readErr error
			if ctx.Err() != nil {
				if direct {
					readErr = collectionFailure(gvr.Resource, ctx.Err())
				} else {
					readErr = namespaceReadFailure(dynamicCache, gvr, namespace)
				}
			} else if direct {
				items, readErr = dynamicCache.ListDirectBounded(ctx, gvr, namespace, limit)
			} else {
				items, readErr = dynamicCache.ListBlockingContext(ctx, gvr, namespace, 3*time.Second)
				if readErr == nil && !dynamicCache.IsNamespaceSynced(gvr, namespace) {
					readErr = namespaceReadFailure(dynamicCache, gvr, namespace)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if readErr != nil {
				var failure *resourceReadFailure
				if !errors.As(readErr, &failure) {
					failure = collectionFailure(gvr.Resource, readErr)
				}
				failures[failure.code] = append(failures[failure.code], namespace)
				if failureByCode[failure.code] == nil {
					failureByCode[failure.code] = failure
				}
				return nil
			}
			read = append(read, namespace)
			if len(rows)+len(items) > limit {
				tooLarge = true
				cancel()
			} else {
				rows = append(rows, items...)
			}
			return nil
		})
	}
	_ = groupReads.Wait()
	if cache != k8s.GetResourceCache() || dynamicCache != k8s.GetDynamicResourceCache() {
		return nil, nil, nil, errReadHandlesChanged
	}
	if tooLarge || len(failures[readreason.ListTooLarge]) > 0 {
		return nil, nil, nil, readFailuref(readreason.ListTooLarge, "Too many resources to load; select fewer namespaces")
	}
	if residualUnknown {
		code := readreason.CollectorScopeLimited
		if direct {
			code = readreason.CollectorForbidden
		}
		gaps = append(gaps, resourceapi.Gap{Code: code})
	}
	for _, code := range slices.Sorted(maps.Keys(failures)) {
		gaps = append(gaps, gapOf(code, failures[code]))
	}
	if len(read) == 0 {
		codes := slices.Collect(maps.Keys(failures))
		code := readreason.Primary(codes)
		failure := failureByCode[code]
		if failure == nil {
			failure = readFailuref(readreason.CollectorScopeLimited, "Radar has no readable scope for %s", kind)
		}
		return nil, nil, nil, &resourceReadFailure{code: failure.code, message: failure.message, gaps: gaps}
	}
	if !residualUnknown && len(failures) == 0 {
		read = nil
	}
	return rows, read, gaps, nil
}

// dynamicCountCoverage reports which of namespaces (nil: every namespace) a
// dynamic kind can be counted in now: read is nil when all of them.
func dynamicCountCoverage(cache *k8s.DynamicResourceCache, gvr schema.GroupVersionResource, namespaces []string, clusterScoped bool) (read []string, gaps []resourceapi.Gap, ok bool) {
	if cache.IsNamespaceSynced(gvr, "") {
		return nil, nil, true
	}
	if clusterScoped {
		return nil, nil, false
	}
	targets := namespaces
	if targets == nil {
		targets = cache.Observation(gvr).Namespaces
	}
	var unread []string
	read = []string{}
	for _, namespace := range targets {
		if cache.IsNamespaceSynced(gvr, namespace) {
			read = append(read, namespace)
		} else {
			unread = append(unread, namespace)
		}
	}
	if len(read) == 0 {
		return nil, nil, false
	}
	if namespaces == nil {
		gaps = append(gaps, resourceapi.Gap{Code: readreason.CollectorScopeLimited})
	} else if len(unread) > 0 {
		gaps = append(gaps, gapOf(readreason.CollectorScopeLimited, unread))
	}
	return read, gaps, true
}

func readDynamicResourceGet(ctx context.Context, cache *k8s.ResourceCache, kind, group, namespace, name string) (*unstructured.Unstructured, error) {
	dynamicCache, gvr, err := dynamicReadHandles(cache, kind, group)
	if err != nil {
		return nil, err
	}
	if k8s.ShouldBypassDynamicInformer(gvr) || (gvr.Group == "apiextensions.k8s.io" && gvr.Resource == "customresourcedefinitions") || (gvr.Group == "apiregistration.k8s.io" && gvr.Resource == "apiservices") {
		resource, err := dynamicCache.GetDirect(ctx, gvr, namespace, name)
		if cache != k8s.GetResourceCache() || dynamicCache != k8s.GetDynamicResourceCache() {
			return nil, errReadHandlesChanged
		}
		return resource, err
	}
	resource, err := dynamicCache.Get(gvr, namespace, name)
	if cache != k8s.GetResourceCache() || dynamicCache != k8s.GetDynamicResourceCache() {
		return nil, errReadHandlesChanged
	}
	if err == nil {
		return resource, nil
	}
	if apierrors.IsUnauthorized(err) || apierrors.IsForbidden(err) {
		return nil, err
	}
	if (apierrors.IsNotFound(err) || errors.Is(err, k8score.ErrResourceNotFound)) && !dynamicCache.IsNamespaceSynced(gvr, namespace) {
		return nil, namespaceReadFailure(dynamicCache, gvr, namespace)
	}
	return resource, err
}

const resourceCountsNamespaceBudget = 2048

// boundCountCoverageDetails caps the namespace samples of a counts response,
// which carries a coverage per kind, so it can't grow with namespaces × kinds.
func boundCountCoverageDetails(coverage map[string]resourceapi.Coverage) {
	sets := 0
	for _, value := range coverage {
		if value.Scope.Namespaces != nil {
			sets++
		}
		if value.Read != nil && value.Read.Namespaces != nil {
			sets++
		}
		for _, gap := range value.Gaps {
			if gap.Namespaces != nil {
				sets++
			}
		}
	}
	if sets == 0 {
		return
	}
	limit := min(resourceapi.RESTSample, resourceCountsNamespaceBudget/sets)
	sample := func(set *resourceapi.NameSet) *resourceapi.NameSet {
		if set == nil {
			return nil
		}
		names := slices.Clone(set.Sample[:min(len(set.Sample), limit)])
		if names == nil {
			names = []string{}
		}
		return &resourceapi.NameSet{Count: set.Count, Sample: names}
	}
	for kind, value := range coverage {
		value.Scope.Namespaces = sample(value.Scope.Namespaces)
		if value.Read != nil {
			read := *value.Read
			read.Namespaces = sample(read.Namespaces)
			value.Read = &read
		}
		value.Gaps = slices.Clone(value.Gaps)
		for i := range value.Gaps {
			value.Gaps[i].Namespaces = sample(value.Gaps[i].Namespaces)
		}
		coverage[kind] = value
	}
}
