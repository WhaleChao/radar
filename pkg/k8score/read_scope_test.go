package k8score

import (
	"slices"
	"testing"
)

func TestAssessKindReadScopeAndReadiness(t *testing.T) {
	ready := true
	rc := &ResourceCache{
		enabledResources:  map[string]bool{Pods: true},
		config:            CacheConfig{ResourceScopes: map[string]ResourceScope{Pods: {Enabled: true, Namespace: "alpha"}}, ResourceScopeNamespaces: map[string][]string{Pods: {"alpha", "beta"}}},
		informerStatuses:  []InformerSyncStatus{{Key: Pods}},
		informerHasSynced: []func() bool{func() bool { return ready }},
	}
	partial := rc.AssessKindRead(Pods, []string{"alpha", "gamma"})
	if partial.Complete || !slices.Equal(partial.Read, []string{"alpha"}) || !slices.Equal(partial.Uncovered, []string{"gamma"}) {
		t.Fatalf("assessment = %+v", partial)
	}
	if !rc.AssessKindRead(Pods, []string{"beta"}).Complete {
		t.Fatal("covered target should be complete")
	}
	empty := rc.AssessKindRead(Pods, []string{})
	if !empty.Complete || empty.Requested == nil || empty.Read == nil || empty.AllNamespaces {
		t.Fatalf("empty finite target became unrestricted: %+v", empty)
	}
	all := rc.AssessKindRead(Pods, nil)
	if all.Complete || all.AllNamespaces || len(all.Read) != 2 {
		t.Fatalf("unscoped assessment = %+v", all)
	}
	ready = false
	if pending := rc.AssessKindRead(Pods, []string{"alpha"}); pending.Complete || pending.Readiness != KindPending || pending.Read != nil {
		t.Fatalf("pending = %+v", pending)
	}
	pendingScope := rc.AssessKindRead(Pods, []string{"alpha", "gamma"})
	if pendingScope.Readiness != KindPending || pendingScope.Read != nil || pendingScope.Complete || !slices.Equal(pendingScope.Uncovered, []string{"gamma"}) {
		t.Fatalf("pending scope = %+v", pendingScope)
	}
	rc.deferredFailed.Store(true)
	rc.deferredSynced = map[string]bool{Pods: false}
	failedScope := rc.AssessKindRead(Pods, []string{"alpha", "gamma"})
	if failedScope.Readiness != KindFailed || failedScope.Read != nil || failedScope.Complete || !slices.Equal(failedScope.Uncovered, []string{"gamma"}) {
		t.Fatalf("failed scope = %+v", failedScope)
	}
	if unavailable := rc.AssessKindRead(Services, nil); unavailable.Complete || unavailable.AllNamespaces {
		t.Fatalf("unavailable = %+v", unavailable)
	}
	var absent *ResourceCache
	if absent.AssessKindRead(Pods, nil).Complete {
		t.Fatal("nil cache established completeness")
	}
	ready = true
	rc.config.ResourceScopes[Pods] = ResourceScope{Enabled: true}
	cluster := rc.AssessKindRead(Pods, nil)
	if !cluster.Complete || !cluster.AllNamespaces {
		t.Fatalf("cluster = %+v", cluster)
	}
}

func TestAssessKindReadRequestedScope(t *testing.T) {
	rc := &ResourceCache{
		enabledResources:  map[string]bool{Pods: true},
		config:            CacheConfig{ResourceScopes: map[string]ResourceScope{Pods: {Enabled: true, Namespace: "alpha"}}, ResourceScopeNamespaces: map[string][]string{Pods: {"alpha", "beta"}}},
		informerStatuses:  []InformerSyncStatus{{Key: Pods}},
		informerHasSynced: []func() bool{func() bool { return true }},
	}
	for _, tt := range []struct {
		name                       string
		requested, read, uncovered []string
		complete                   bool
	}{
		{"all", nil, []string{"alpha", "beta"}, nil, false},
		{"explicit order", []string{"beta", "alpha"}, []string{"beta", "alpha"}, nil, true},
		{"partial", []string{"beta", "gamma"}, []string{"beta"}, []string{"gamma"}, false},
		{"zero overlap", []string{"gamma"}, []string{}, []string{"gamma"}, false},
		{"no target", []string{}, []string{}, nil, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := rc.AssessKindRead(Pods, tt.requested)
			if got.Readiness != KindReady || got.Complete != tt.complete || !slices.Equal(got.Read, tt.read) || !slices.Equal(got.Uncovered, tt.uncovered) {
				t.Fatalf("assessment = %+v", got)
			}
			if tt.requested != nil && got.Read == nil {
				t.Fatal("finite target became all namespaces")
			}
		})
	}
	rc.config.ResourceScopes[Pods] = ResourceScope{Enabled: true}
	if got := rc.AssessKindRead(Pods, []string{}); !got.Complete || !got.AllNamespaces || got.Read == nil || len(got.Read) != 0 {
		t.Fatalf("cluster-wide no-target = %+v", got)
	}
	rc.informerHasSynced = []func() bool{func() bool { return false }}
	if got := rc.AssessKindRead(Pods, []string{"alpha"}); got.Readiness != KindPending || !got.AllNamespaces || got.Read != nil || got.Complete {
		t.Fatalf("cluster-wide pending = %+v", got)
	}
}
