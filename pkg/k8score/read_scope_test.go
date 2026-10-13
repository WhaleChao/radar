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
	all := rc.AssessKindRead(Pods, nil)
	if all.Complete || all.AllNamespaces || len(all.Read) != 2 {
		t.Fatalf("unscoped assessment = %+v", all)
	}
	ready = false
	if pending := rc.AssessKindRead(Pods, []string{"alpha"}); pending.Complete || pending.Readiness != KindPending || pending.Read != nil {
		t.Fatalf("pending = %+v", pending)
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
