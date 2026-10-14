package server

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/skyhook-io/radar/internal/k8s"
	"github.com/skyhook-io/radar/pkg/capacityapi"
	"github.com/skyhook-io/radar/pkg/k8score"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCapacityMixedScopeKeepsKnownExclusionsWhileUnread(t *testing.T) {
	for _, state := range []struct {
		readiness k8s.KindReadiness
		timeout   time.Duration
		status    capacityapi.CoverageStatus
	}{
		{k8s.KindPending, time.Minute, capacityapi.CoverageSyncing},
		{k8s.KindFailed, time.Millisecond, capacityapi.CoverageError},
	} {
		core, err := k8score.NewResourceCache(k8score.CacheConfig{Client: fake.NewClientset(), ResourceScopes: map[string]k8score.ResourceScope{"services": {Enabled: true}, "pods": {Enabled: true, Namespace: "alpha"}}, DeferredTypes: map[string]bool{"pods": true}, DebugSyncDelays: map[string]time.Duration{"pods": time.Hour}, DeferredSyncTimeout: state.timeout})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(core.Stop)
		waitFor(t, 5*time.Second, func() bool { return core.KindReadinessFor("pods") == state.readiness })
		cache := k8s.GetResourceCache()
		prior := cache.ResourceCache
		cache.ResourceCache = core
		meta := capacityapi.NewResponseMeta(time.Now())
		pods := (&Server{}).loadCapacityPods(httptest.NewRequest(http.MethodGet, "/api/capacity?namespaces=alpha,beta", nil), &meta, time.Now())
		cache.ResourceCache = prior
		coverage := meta.Coverage[capacityapi.CoveragePods]
		if len(pods) != 0 || coverage.Status != state.status || coverage.ReasonCode != "pod_cache_scope_partial" || !slices.Equal(coverage.Namespaces, []string{"alpha"}) || coverage.ItemCount != nil || coverage.ObservedAt != nil {
			t.Fatalf("unread mixed scope: %+v rows=%d", coverage, len(pods))
		}
	}
}
