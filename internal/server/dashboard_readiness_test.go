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
