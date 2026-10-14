package server

import (
	"errors"
	"log"
	"net/http"
	"slices"

	"github.com/skyhook-io/radar/internal/k8s"
	"github.com/skyhook-io/radar/pkg/k8score"
	"github.com/skyhook-io/radar/pkg/resourceapi"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type ResourceCountsResponse struct {
	Counts      map[string]int                  `json:"counts"`
	Coverage    map[string]resourceapi.Coverage `json:"coverage"`
	Forbidden   []string                        `json:"forbidden,omitempty"`
	Unavailable []string                        `json:"unavailable,omitempty"`
	// Reasons maps a forbidden kind key to why it's hidden:
	//   "rbac_denied" — Radar's ServiceAccount can read the kind but the user's
	//      own RBAC denies it. Granting the user list access surfaces it.
	//   "unavailable" — Radar can't read the kind at all (no informer): its type
	//      isn't installed, the SA lacks RBAC, or the feature is off (e.g.
	//      rbac.viewRBAC). A user-level grant won't help.
	Reasons map[string]string `json:"reasons,omitempty"`
}

const (
	reasonRBACDenied  = "rbac_denied"
	reasonUnavailable = "unavailable"
)

const (
	endpointSliceCountKey          = "discovery.k8s.io/EndpointSlice"
	endpointSliceCountNamespaceCap = 50
	endpointSliceCountConcurrency  = 8
)

var featuredKubernetesAPIs = map[string]struct{}{
	"scheduling.k8s.io/Workload":                {},
	"scheduling.k8s.io/PodGroup":                {},
	"scheduling.k8s.io/CompositePodGroup":       {},
	"certificates.k8s.io/PodCertificateRequest": {},
	"certificates.k8s.io/ClusterTrustBundle":    {},
}

func isFeaturedKubernetesAPI(group, kind string) bool {
	_, ok := featuredKubernetesAPIs[group+"/"+kind]
	return ok
}

func (s *Server) handleResourceCounts(w http.ResponseWriter, r *http.Request) {
	// Served during the progressive shell too: the sidebar badges and the
	// large-list guard need counts as kinds become ready, not after full
	// connect. Kinds whose informer has not synced report as unavailable —
	// a zero from an empty store would read as "the cluster has none" and
	// would unlatch the large-list guard on exactly the clusters it protects.
	if !s.requireConnectedOrSyncing(w) {
		return
	}

	namespaces := s.parseNamespacesForUser(r)

	cache, syncing := k8s.SnapshotCaches()
	if cache == nil {
		cache = syncing
	}
	if cache == nil {
		s.writeNotConnected(w)
		return
	}

	dynamicCache := k8s.GetDynamicResourceCache()
	discovery := k8s.GetResourceDiscovery()
	counts := make(map[string]int)
	countCoverage := make(map[string]resourceapi.Coverage)
	var forbidden []string
	var unavailable []string
	reasons := map[string]string{}
	covered := map[string]bool{}
	forbiddenSeen := map[string]bool{}
	unavailableSeen := map[string]bool{}

	countKey := func(group, kind string) string {
		if group != "" {
			return group + "/" + kind
		}
		return kind
	}
	markCounted := func(key string, count int) {
		if covered[key] {
			return
		}
		counts[key] = count
		covered[key] = true
	}
	markForbidden := func(key, reason string) {
		if covered[key] {
			return
		}
		if !forbiddenSeen[key] {
			forbidden = append(forbidden, key)
			forbiddenSeen[key] = true
		}
		reasons[key] = reason
		covered[key] = true
	}
	markUnavailable := func(key string) {
		if covered[key] {
			return
		}
		if !unavailableSeen[key] {
			unavailable = append(unavailable, key)
			unavailableSeen[key] = true
		}
		covered[key] = true
	}
	countEndpointSlices := func() {
		if noNamespaceAccess(namespaces) {
			markForbidden(endpointSliceCountKey, reasonRBACDenied)
			return
		}
		if covered[endpointSliceCountKey] {
			return
		}
		if dynamicCache == nil {
			markUnavailable(endpointSliceCountKey)
			return
		}
		gvr, ok := k8s.BuiltinGVR("endpointslices", "discovery.k8s.io")
		if !ok {
			markUnavailable(endpointSliceCountKey)
			return
		}
		total, err := dynamicCache.CountDirectProbe(r.Context(), gvr, namespaces, endpointSliceCountNamespaceCap, endpointSliceCountConcurrency)
		if err != nil {
			markUnavailable(endpointSliceCountKey)
			if !errors.Is(err, k8score.ErrResourceCountUnavailable) {
				log.Printf("[resource-counts] Failed to count EndpointSlice: %v", err)
			}
			return
		}
		markCounted(endpointSliceCountKey, total)
		countCoverage[endpointSliceCountKey] = readCoverage(namespaces, namespaces, false, true, nil)
	}

	for _, kl := range k8score.AllKindListers() {
		if cache.KindReadinessForKindName(kl.Kind()) == k8score.KindUnavailable {
			markForbidden(kl.CountKey(), reasonUnavailable)
			continue
		}
		kindNamespaces, _, _, allowed := s.preflightResourceList(r, normalizeKind(kl.Kind()), "", namespaces)
		if !allowed {
			markForbidden(kl.CountKey(), reasonRBACDenied)
			continue
		}
		clusterScoped, _, _ := k8s.ClassifyKindScope(kl.Kind(), "")
		// An unsynced informer has a partial (or empty) store — its count is
		// not a fact yet. Unavailable keeps the sidebar badge at "–" and the
		// large-list guard latched. A terminally-failed kind carries a reason
		// so the client can hand it to the list endpoint, whose 503
		// kind_sync_failed renders honestly (the guard would otherwise trap
		// it on "count unavailable" forever).
		switch cache.KindReadinessForKindName(kl.Kind()) {
		case k8score.KindPending, k8score.KindFailed:
			markUnavailable(kl.CountKey())
			coverage, _, err := typedReadCoverage(cache, informerKeyForKind(kl.Kind()), kindNamespaces, clusterScoped)
			var failure *resourceReadFailure
			if errors.As(err, &failure) {
				reasons[kl.CountKey()] = string(failure.reason)
			}
			if coverage.Status != "" {
				countCoverage[kl.CountKey()] = coverage
			}
			continue
		}
		l := kl.Lister()(cache.ResourceCache)
		if l == nil {
			// No informer: Radar's SA can't read this kind (not installed, SA
			// RBAC, or feature off) — a user-level grant won't surface it.
			markForbidden(kl.CountKey(), reasonUnavailable)
			continue
		}

		coverage, readNamespaces, scopeErr := typedReadCoverage(cache, informerKeyForKind(kl.Kind()), kindNamespaces, clusterScoped)
		if scopeErr != nil {
			markUnavailable(kl.CountKey())
			var failure *resourceReadFailure
			if errors.As(scopeErr, &failure) {
				reasons[kl.CountKey()] = string(failure.reason)
			}
			continue
		}
		n := k8score.ListCountNamespaced(l, readNamespaces)
		markCounted(kl.CountKey(), n)
		countCoverage[kl.CountKey()] = withCallerScopeGaps(coverage, namespaces, kindNamespaces)
	}

	// 2. Discovery-backed resources.
	if discovery != nil && dynamicCache != nil {
		resources, err := discovery.GetAPIResources()
		if err != nil {
			log.Printf("[resource-counts] Failed to discover API resources for CRD counts: %v", err)
		} else {
			type discoveredInfo struct {
				kind       string
				group      string
				resource   string
				version    string
				namespaced bool
				gvr        schema.GroupVersionResource
			}

			browseResources := make(map[string]discoveredInfo)
			var browseOrder []string
			for _, res := range resources {
				if !isFeaturedKubernetesAPI(res.Group, res.Kind) || !slices.Contains(res.Verbs, "list") {
					continue
				}
				key := countKey(res.Group, res.Kind)
				info := discoveredInfo{
					kind:       res.Kind,
					group:      res.Group,
					resource:   res.Name,
					version:    res.Version,
					namespaced: res.Namespaced,
					gvr:        schema.GroupVersionResource{Group: res.Group, Version: res.Version, Resource: res.Name},
				}
				if existing, ok := browseResources[key]; !ok {
					browseOrder = append(browseOrder, key)
					browseResources[key] = info
				} else if k8score.IsMoreStableVersion(res.Version, existing.version) {
					browseResources[key] = info
				}
			}

			for _, key := range browseOrder {
				resource := browseResources[key]
				if resource.namespaced && noNamespaceAccess(namespaces) {
					markForbidden(key, reasonRBACDenied)
					continue
				}
				if resource.namespaced && len(namespaces) > endpointSliceCountNamespaceCap {
					markUnavailable(key)
					continue
				}
				authorized := true
				if !resource.namespaced || len(namespaces) == 0 {
					authorized = s.canRead(r, resource.group, resource.resource, "", "list")
				} else {
					for _, namespace := range namespaces {
						if !s.canRead(r, resource.group, resource.resource, namespace, "list") {
							authorized = false
							break
						}
					}
				}
				if !authorized {
					markForbidden(key, reasonRBACDenied)
					continue
				}

				countNamespaces := namespaces
				if !resource.namespaced {
					countNamespaces = nil
				}
				total, err := dynamicCache.CountDirectProbe(r.Context(), resource.gvr, countNamespaces, endpointSliceCountNamespaceCap, endpointSliceCountConcurrency)
				if err != nil {
					markUnavailable(key)
					if !errors.Is(err, k8score.ErrResourceCountUnavailable) {
						log.Printf("[resource-counts] Failed to count %s: %v", key, err)
					}
					continue
				}
				markCounted(key, total)
				countCoverage[key] = readCoverage(countNamespaces, countNamespaces, !resource.namespaced, true, nil)
			}

			// Deduplicate CRDs by group+kind, keeping the most stable served version.
			crdSeen := make(map[string]bool)
			crds := make(map[string]discoveredInfo)
			var crdOrder []string
			for _, res := range resources {
				if !res.IsCRD {
					continue
				}
				// Informer-backed counts only work for listable+watchable kinds.
				// Create-only review resources (LocalSubjectAccessReview, etc.)
				// never sync an informer and would log a permanent count error.
				if !slices.Contains(res.Verbs, "list") || !slices.Contains(res.Verbs, "watch") {
					continue
				}
				key := countKey(res.Group, res.Kind)
				info := discoveredInfo{
					kind:       res.Kind,
					group:      res.Group,
					resource:   res.Name,
					version:    res.Version,
					namespaced: res.Namespaced,
					gvr:        schema.GroupVersionResource{Group: res.Group, Version: res.Version, Resource: res.Name},
				}
				if !crdSeen[key] {
					crdSeen[key] = true
					crdOrder = append(crdOrder, key)
					crds[key] = info
				} else if k8score.IsMoreStableVersion(res.Version, crds[key].version) {
					crds[key] = info
				}
			}

			for _, key := range crdOrder {
				crd := crds[key]
				if crd.namespaced && noNamespaceAccess(namespaces) {
					markForbidden(key, reasonRBACDenied)
					continue
				}
				if !crd.namespaced && !s.canRead(r, crd.group, crd.resource, "", "list") {
					markForbidden(key, reasonRBACDenied)
					continue
				}
				target := namespaces
				if !crd.namespaced {
					target = nil
				}
				coverage, readNamespaces, ready := dynamicCountCoverage(dynamicCache, crd.gvr, target, !crd.namespaced)
				if ready {
					if n, err := dynamicCache.Count(crd.gvr, readNamespaces); err == nil {
						markCounted(key, n)
						countCoverage[key] = coverage
						continue
					}
				}
				markUnavailable(key)
			}
		}
	}
	countEndpointSlices()

	connectedCache, syncingCache := k8s.SnapshotCaches()
	if (cache != connectedCache && cache != syncingCache) || dynamicCache != k8s.GetDynamicResourceCache() || discovery != k8s.GetResourceDiscovery() {
		s.writeCollectionFailure(w, "resource counts", k8s.ErrDynamicNotReady)
		return
	}
	boundCountCoverageDetails(countCoverage)
	s.writeJSON(w, ResourceCountsResponse{
		Counts:      counts,
		Coverage:    countCoverage,
		Forbidden:   forbidden,
		Unavailable: unavailable,
		Reasons:     reasons,
	})
}
