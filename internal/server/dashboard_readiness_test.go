package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/skyhook-io/radar/internal/k8s"
	"github.com/skyhook-io/radar/pkg/capacityapi"
	"github.com/skyhook-io/radar/pkg/k8score"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestDashboardAndVitalsWithholdUnobservedRequests(t *testing.T) {
	client := fake.NewClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}, Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			corev1.ResourceCPU: mustQty(t, "4"), corev1.ResourceMemory: mustQty(t, "8Gi"),
		}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "pg"}, Spec: corev1.PodSpec{
			NodeName: "node-1", Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU: mustQty(t, "500m"), corev1.ResourceMemory: mustQty(t, "1Gi"),
			}}}},
		}},
	)
	release := make(chan struct{})
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		select {
		case <-release:
			return false, nil, nil
		default:
			return true, nil, errors.New("initial list held until test release")
		}
	})
	t.Cleanup(func() {
		k8s.ResetResourceCache()
		if err := k8s.InitTestResourceCache(testFakeClient); err != nil {
			t.Fatalf("restore package fixture cache: %v", err)
		}
	})
	k8s.ResetResourceCache()
	if err := k8s.InitTestPromotedSyncingCache(client, 5*time.Second, 3*time.Second, nil); err != nil {
		t.Fatal(err)
	}
	cache := k8s.GetResourceCache()
	srv := &Server{}
	for _, readiness := range []k8s.KindReadiness{k8s.KindPending, k8s.KindFailed, k8s.KindReady} {
		if readiness == k8s.KindReady {
			close(release)
		}
		waitFor(t, 45*time.Second, func() bool {
			return cache.KindReadinessFor("pods") == readiness && cache.KindReadinessFor("nodes") == k8s.KindReady &&
				(readiness != k8s.KindReady || len(cache.PendingPromotedKinds()) == 0)
		})
		metrics := srv.getDashboardMetrics(context.Background(), nil)
		w := httptest.NewRecorder()
		srv.handleVitals(w, httptest.NewRequest(http.MethodGet, "/api/vitals", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("vitals status = %d: %s", w.Code, w.Body.String())
		}
		var vitals VitalsResponse
		if err := json.Unmarshal(w.Body.Bytes(), &vitals); err != nil {
			t.Fatal(err)
		}
		if readiness != k8s.KindReady {
			if metrics != nil || vitals.CPU != nil || vitals.Memory != nil {
				t.Fatalf("readiness=%v emitted requests: dashboard=%#v CPU=%#v memory=%#v", readiness, metrics, vitals.CPU, vitals.Memory)
			}
			if vitals.Completeness.Complete || !slices.Contains(vitals.Completeness.Pending, "Pod") {
				t.Fatalf("unready Pods lost existing completeness caveat: %#v", vitals.Completeness)
			}
			continue
		}
		if metrics == nil || metrics.CPU == nil || metrics.Memory == nil || metrics.CPU.RequestsMillis != 500 || metrics.Memory.RequestsMillis != 1024 {
			t.Fatalf("ready dashboard did not restore actual requests: %#v", metrics)
		}
		if vitals.CPU == nil || vitals.Memory == nil || vitals.CPU.RequestsMillis != 500 || vitals.Memory.RequestsMillis != 1024 || vitals.Pods.Total != 1 || !vitals.Completeness.Complete {
			t.Fatalf("ready vitals = %#v", vitals)
		}
	}
}

func TestDashboardAndVitalsRequireCoverageOfTheirRequestedScope(t *testing.T) {
	client := fake.NewClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}, Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: mustQty(t, "4"), corev1.ResourceMemory: mustQty(t, "8Gi")}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "alpha"}, Spec: corev1.PodSpec{NodeName: "node-1", Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: mustQty(t, "500m")}}}}}},
	)
	t.Cleanup(func() {
		k8s.ResetResourceCache()
		if err := k8s.InitTestResourceCache(testFakeClient); err != nil {
			t.Fatal(err)
		}
	})
	k8s.ResetResourceCache()
	if err := k8s.InitScopedTestResourceCache(client, map[string]k8score.ResourceScope{"nodes": {Enabled: true}, "pods": {Enabled: true, Namespace: "alpha"}}); err != nil {
		t.Fatal(err)
	}
	srv := &Server{}
	for _, tc := range []struct {
		name, query string
		namespaces  []string
		complete    bool
	}{
		{"unscoped", "", nil, false},
		{"covered", "?namespaces=alpha", []string{"alpha"}, true},
		{"mixed", "?namespaces=alpha,beta", []string{"alpha", "beta"}, false},
		{"excluded", "?namespaces=beta", []string{"beta"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metrics := srv.getDashboardMetrics(context.Background(), tc.namespaces)
			w := httptest.NewRecorder()
			srv.handleVitals(w, httptest.NewRequest(http.MethodGet, "/api/vitals"+tc.query, nil))
			var vitals VitalsResponse
			if w.Code != 200 {
				t.Fatalf("status=%d: %s", w.Code, w.Body.String())
			}
			if err := json.Unmarshal(w.Body.Bytes(), &vitals); err != nil {
				t.Fatal(err)
			}
			if tc.complete {
				if metrics == nil || metrics.CPU == nil || metrics.CPU.RequestsMillis != 500 || vitals.CPU == nil || vitals.CPU.RequestsMillis != 500 || !vitals.Completeness.Complete {
					t.Fatalf("complete scope missing metrics: %+v %+v", metrics, vitals)
				}
			} else if metrics != nil || vitals.CPU != nil || vitals.Memory != nil || vitals.Completeness.Complete || !slices.Contains(vitals.Completeness.Restricted, "Pod") {
				t.Fatalf("incomplete scope presented aggregate: %+v %+v", metrics, vitals)
			}
		})
	}
}

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
