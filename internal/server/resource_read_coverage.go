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

func resourceScope(namespaces []string, clusterScoped bool) resourceapi.Scope {
	if clusterScoped {
		return resourceapi.Scope{Type: resourceapi.ScopeCluster}
	}
	if namespaces == nil {
		return resourceapi.Scope{Type: resourceapi.ScopeAllNamespaces}
	}
	return resourceapi.Scope{Type: resourceapi.ScopeNamespaces, Namespaces: resourceapi.NamespacePreview(namespaces, resourceapi.RESTNamespacePreview)}
}

func readCoverage(requested, read []string, clusterScoped, complete bool, gaps []resourceapi.Gap) resourceapi.Coverage {
	status := resourceapi.CoveragePartial
	if complete {
		status = resourceapi.CoverageComplete
	}
	if gaps == nil {
		gaps = []resourceapi.Gap{}
	}
	return resourceapi.Coverage{Status: status, Requested: resourceScope(requested, clusterScoped), Read: resourceScope(read, clusterScoped), Gaps: gaps}
}

type resourceReadFailure struct {
	status  int
	reason  readreason.Code
	message string
}

func (e *resourceReadFailure) Error() string { return e.message }

func collectionFailure(resource string, err error) *resourceReadFailure {
	switch {
	case errors.Is(err, k8score.ErrResourceListTooLarge):
		return &resourceReadFailure{400, "resource_list_too_large", "Too many resources to load; select fewer namespaces"}
	case apierrors.IsUnauthorized(err):
		return &resourceReadFailure{http.StatusServiceUnavailable, readreason.CollectorUnauthorized, "Kubernetes rejected Radar's collector credentials"}
	case apierrors.IsForbidden(err):
		return &resourceReadFailure{http.StatusForbidden, readreason.CollectorAccessDenied, fmt.Sprintf("Radar's collector cannot read %s in the requested scope", resource)}
	case errors.Is(err, k8s.ErrUnknownDynamicKind):
		return &resourceReadFailure{http.StatusBadRequest, readreason.UnknownKind, err.Error()}
	case errors.Is(err, k8s.ErrDynamicNotReady):
		return &resourceReadFailure{http.StatusServiceUnavailable, readreason.KindSyncPending, "Resource discovery is still loading"}
	default:
		return &resourceReadFailure{http.StatusInternalServerError, readreason.ReadError, fmt.Sprintf("Radar could not read %s", resource)}
	}
}

func (s *Server) writeCollectionFailure(w http.ResponseWriter, resource string, err error) {
	var failure *resourceReadFailure
	if !errors.As(err, &failure) {
		failure = collectionFailure(resource, err)
	}
	if failure.status == http.StatusInternalServerError {
		log.Printf("[resource-read] Failed to read %s: %v", resource, err)
	}
	s.writeErrorCode(w, failure.status, string(failure.reason), failure.message)
}

func typedReadCoverage(cache *k8s.ResourceCache, resource string, namespaces []string, clusterScoped bool) (resourceapi.Coverage, []string, error) {
	assessment := cache.AssessKindRead(resource, namespaces)
	switch assessment.Readiness {
	case k8score.KindPending:
		return resourceapi.Coverage{}, nil, &resourceReadFailure{503, readreason.KindSyncPending, fmt.Sprintf("%s are still loading, please retry shortly", resource)}
	case k8score.KindFailed:
		return resourceapi.Coverage{}, nil, &resourceReadFailure{503, readreason.KindSyncFailed, fmt.Sprintf("%s failed to load within the sync deadline", resource)}
	case k8score.KindUnavailable:
		if permissions := cache.PermissionResult(); permissions != nil {
			if scope, known := permissions.Scopes[resource]; known && !scope.Enabled {
				failure := collectionFailure(resource, permissions.ProbeErrors[resource])
				if failure.reason == readreason.ReadError {
					failure = &resourceReadFailure{403, readreason.CollectorAccessDenied, fmt.Sprintf("Radar's collector cannot list/watch %s", resource)}
				}
				return resourceapi.Coverage{}, nil, failure
			}
		}
		return resourceapi.Coverage{}, nil, &resourceReadFailure{503, readreason.KindNotWatched, fmt.Sprintf("Radar is not watching %s", resource)}
	}
	if !assessment.Complete && len(assessment.Read) == 0 {
		return resourceapi.Coverage{}, nil, &resourceReadFailure{503, readreason.CollectorScopeLimited, fmt.Sprintf("Radar's collector does not cover the requested scope for %s", resource)}
	}
	var gaps []resourceapi.Gap
	if !assessment.Complete {
		gap := resourceapi.Gap{Reason: readreason.CollectorScopeLimited}
		if namespaces != nil {
			gap.Namespaces = resourceapi.NamespacePreview(assessment.Uncovered, resourceapi.RESTNamespacePreview)
		}
		gaps = append(gaps, gap)
	}
	return readCoverage(namespaces, assessment.Read, clusterScoped, assessment.Complete, gaps), assessment.Read, nil
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
	s.writeJSON(w, map[string]any{"error": "Too many resources to load; select fewer namespaces", "error_code": "resource_list_too_large", "count": count, "limit": limit})
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
		return &resourceReadFailure{503, readreason.KindSyncFailed, fmt.Sprintf("%s initial cache sync has stalled", gvr.Resource)}
	case observation.State == k8score.DynamicObservationDenied:
		return &resourceReadFailure{403, readreason.CollectorAccessDenied, fmt.Sprintf("Radar's collector cannot read %s in the requested scope", gvr.Resource)}
	case observation.State == k8score.DynamicObservationUnwatched && dynamicCache.Observation(gvr).State == k8score.DynamicObservationSynced:
		return &resourceReadFailure{503, readreason.CollectorScopeLimited, fmt.Sprintf("Radar's collector does not cover the requested scope for %s", gvr.Resource)}
	case observation.State == k8score.DynamicObservationUnsupported:
		return &resourceReadFailure{503, readreason.CollectionNotSupported, fmt.Sprintf("%s cannot be collected with list/watch", gvr.Resource)}
	default:
		return &resourceReadFailure{503, readreason.KindSyncPending, fmt.Sprintf("%s are still loading, please retry shortly", gvr.Resource)}
	}
}

func readDynamicResourceList(r *http.Request, cache *k8s.ResourceCache, kind, group string, namespaces []string, limit int) ([]*unstructured.Unstructured, resourceapi.Coverage, error) {
	dynamicCache, gvr, err := dynamicReadHandles(cache, kind, group)
	if err != nil {
		return nil, resourceapi.Coverage{}, err
	}
	clusterScoped, _, _ := k8s.ClassifyKindScope(kind, group)
	direct := k8s.ShouldBypassDynamicInformer(gvr)
	targets := namespaces
	residualUnknown := false
	var rows []*unstructured.Unstructured
	if namespaces == nil {
		if direct {
			items, listErr := dynamicCache.ListDirectBounded(r.Context(), gvr, "", limit)
			if listErr == nil {
				return items, readCoverage(nil, nil, clusterScoped, true, nil), nil
			}
			if !apierrors.IsForbidden(listErr) || clusterScoped {
				return nil, resourceapi.Coverage{}, listErr
			}
			targets = dynamicCache.FallbackNamespaces()
		} else {
			items, listErr := dynamicCache.ListContext(r.Context(), gvr, "")
			if listErr != nil {
				return nil, resourceapi.Coverage{}, listErr
			}
			if dynamicCache.IsNamespaceSynced(gvr, "") {
				return items, readCoverage(nil, nil, clusterScoped, true, nil), nil
			}
			observation := dynamicCache.Observation(gvr)
			if observation.Scope != k8score.DynamicObservationScopeExplicitNamespaces {
				return nil, resourceapi.Coverage{}, namespaceReadFailure(dynamicCache, gvr, "")
			}
			targets = observation.Namespaces
		}
		residualUnknown = true
	}
	targets = slices.Compact(slices.Sorted(slices.Values(targets)))
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	var mu sync.Mutex
	var read []string
	failures := make(map[readreason.Code][]string)
	var firstFailure error
	var groupReads errgroup.Group
	groupReads.SetLimit(8)
	for _, namespace := range targets {
		groupReads.Go(func() error {
			var items []*unstructured.Unstructured
			var readErr error
			if ctx.Err() != nil {
				readErr = namespaceReadFailure(dynamicCache, gvr, namespace)
			} else if direct {
				items, readErr = dynamicCache.ListDirectBounded(ctx, gvr, namespace, limit)
			} else {
				items, readErr = dynamicCache.ListContext(ctx, gvr, namespace)
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
				failures[failure.reason] = append(failures[failure.reason], namespace)
				if firstFailure == nil {
					firstFailure = failure
				}
			} else {
				read = append(read, namespace)
				if len(rows)+len(items) > limit {
					failures["resource_list_too_large"] = append(failures["resource_list_too_large"], namespace)
					cancel()
				} else {
					rows = append(rows, items...)
				}
			}
			return nil
		})
	}
	_ = groupReads.Wait()
	if len(read) == 0 {
		if firstFailure != nil {
			return nil, resourceapi.Coverage{}, firstFailure
		}
		return nil, resourceapi.Coverage{}, &resourceReadFailure{503, readreason.CollectorScopeLimited, fmt.Sprintf("Radar has no readable scope for %s", kind)}
	}
	if rejected := failures[readreason.CollectorUnauthorized]; len(rejected) > 0 {
		return nil, resourceapi.Coverage{}, &resourceReadFailure{503, readreason.CollectorUnauthorized, "Kubernetes rejected Radar's collector credentials"}
	}
	if tooLarge, exists := failures["resource_list_too_large"]; exists && len(tooLarge) > 0 {
		return nil, resourceapi.Coverage{}, &resourceReadFailure{400, "resource_list_too_large", "Too many resources to load; select fewer namespaces"}
	}
	var gaps []resourceapi.Gap
	if residualUnknown {
		reason := readreason.CollectorScopeLimited
		if direct {
			reason = readreason.CollectorAccessDenied
		}
		gaps = append(gaps, resourceapi.Gap{Reason: reason})
	}
	reasons := slices.Sorted(maps.Keys(failures))
	for _, reason := range reasons {
		gaps = append(gaps, resourceapi.Gap{Reason: reason, Namespaces: resourceapi.NamespacePreview(failures[reason], resourceapi.RESTNamespacePreview)})
	}
	return rows, readCoverage(namespaces, read, clusterScoped, len(gaps) == 0, gaps), nil
}

func dynamicCountCoverage(cache *k8s.DynamicResourceCache, gvr schema.GroupVersionResource, namespaces []string, clusterScoped bool) (resourceapi.Coverage, []string, bool) {
	if cache.IsNamespaceSynced(gvr, "") {
		return readCoverage(namespaces, namespaces, clusterScoped, true, nil), namespaces, true
	}
	if clusterScoped {
		return resourceapi.Coverage{}, nil, false
	}
	targets := namespaces
	if targets == nil {
		targets = cache.Observation(gvr).Namespaces
	}
	var read, unread []string
	for _, namespace := range targets {
		if cache.IsNamespaceSynced(gvr, namespace) {
			read = append(read, namespace)
		} else {
			unread = append(unread, namespace)
		}
	}
	if len(read) == 0 {
		return resourceapi.Coverage{}, nil, false
	}
	var gaps []resourceapi.Gap
	if namespaces == nil {
		gaps = append(gaps, resourceapi.Gap{Reason: readreason.CollectorScopeLimited})
	} else if len(unread) > 0 {
		gaps = append(gaps, resourceapi.Gap{Reason: readreason.CollectorScopeLimited, Namespaces: resourceapi.NamespacePreview(unread, resourceapi.RESTNamespacePreview)})
	}
	return readCoverage(namespaces, read, false, len(gaps) == 0, gaps), read, true
}

func readDynamicResourceGet(ctx context.Context, cache *k8s.ResourceCache, kind, group, namespace, name string) (*unstructured.Unstructured, error) {
	dynamicCache, gvr, err := dynamicReadHandles(cache, kind, group)
	if err != nil {
		return nil, err
	}
	if k8s.ShouldBypassDynamicInformer(gvr) || (gvr.Group == "apiextensions.k8s.io" && gvr.Resource == "customresourcedefinitions") || (gvr.Group == "apiregistration.k8s.io" && gvr.Resource == "apiservices") {
		return dynamicCache.GetDirect(ctx, gvr, namespace, name)
	}
	resource, err := dynamicCache.Get(gvr, namespace, name)
	if apierrors.IsUnauthorized(err) || apierrors.IsForbidden(err) {
		return nil, err
	}
	if (err == nil || apierrors.IsNotFound(err) || errors.Is(err, k8score.ErrResourceNotFound)) && !dynamicCache.IsNamespaceSynced(gvr, namespace) {
		return nil, namespaceReadFailure(dynamicCache, gvr, namespace)
	}
	return resource, err
}

const resourceCountsNamespaceBudget = 2048

func boundCountCoverageDetails(coverage map[string]resourceapi.Coverage) {
	sets := 0
	for _, value := range coverage {
		if value.Requested.Namespaces != nil {
			sets++
		}
		if value.Read.Namespaces != nil {
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
	limit := min(resourceapi.RESTNamespacePreview, resourceCountsNamespaceBudget/sets)
	preview := func(set *resourceapi.NamespaceSet) *resourceapi.NamespaceSet {
		if set == nil {
			return nil
		}
		names := slices.Clone(set.Names[:min(len(set.Names), limit)])
		if names == nil {
			names = []string{}
		}
		return &resourceapi.NamespaceSet{Names: names, Total: set.Total, Omitted: set.Total - len(names)}
	}
	for kind, value := range coverage {
		value.Requested.Namespaces = preview(value.Requested.Namespaces)
		value.Read.Namespaces = preview(value.Read.Namespaces)
		value.Gaps = slices.Clone(value.Gaps)
		for i := range value.Gaps {
			value.Gaps[i].Namespaces = preview(value.Gaps[i].Namespaces)
		}
		coverage[kind] = value
	}
}

// The namespace target here was already visible to the caller before exact
// kind authorization filtered it, so reporting withheld target names leaks no
// collector inventory or hidden-tenant counts.
func withCallerScopeGaps(coverage resourceapi.Coverage, requested, authorized []string) resourceapi.Coverage {
	if requested == nil || coverage.Requested.Type == resourceapi.ScopeCluster {
		return coverage
	}
	visible := make(map[string]bool, len(authorized))
	for _, namespace := range authorized {
		visible[namespace] = true
	}
	var withheld []string
	for _, namespace := range requested {
		if !visible[namespace] {
			withheld = append(withheld, namespace)
		}
	}
	if len(withheld) == 0 {
		return coverage
	}
	coverage.Status = resourceapi.CoveragePartial
	coverage.Requested = resourceScope(requested, false)
	coverage.Gaps = append(coverage.Gaps, resourceapi.Gap{Reason: readreason.RBACDenied, Namespaces: resourceapi.NamespacePreview(withheld, resourceapi.RESTNamespacePreview)})
	return coverage
}
