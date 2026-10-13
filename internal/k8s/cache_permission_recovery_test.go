package k8s

import (
	"context"
	"testing"
	"time"

	"github.com/skyhook-io/radar/pkg/k8score"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestBootstrapResourcePermissionsRetriesNamespace401BeforeWiring(t *testing.T) {
	rejected := &PermissionCheckResult{Perms: &ResourcePermissions{Pods: true}, Scopes: map[string]k8score.ResourceScope{"pods": {Enabled: true, Namespace: "beta"}}, NamespaceProbeErrors: map[string]map[string]error{"pods": {"alpha": apierrors.NewUnauthorized("expired startup token")}}}
	recovered := &PermissionCheckResult{Perms: &ResourcePermissions{Pods: true}, Scopes: map[string]k8score.ResourceScope{"pods": {Enabled: true}}, ScopeCandidates: []string{"alpha", "beta"}}
	calls := 0
	result, err := bootstrapResourcePermissions(context.Background(), func(ctx context.Context, retry bool) (*PermissionCheckResult, bool) {
		calls++
		if retry != (calls == 2) {
			t.Fatalf("retry=%v at call %d", retry, calls)
		}
		if calls == 1 {
			return rejected, true
		}
		return recovered, true
	})
	if err != nil || result != recovered || calls != 2 {
		t.Fatalf("result=%+v error=%v calls=%d", result, err, calls)
	}
}

func TestBootstrapResourcePermissionsPersistent401FailsIntoAuthClassification(t *testing.T) {
	for _, namespaceFailure := range []bool{false, true} {
		result := &PermissionCheckResult{Perms: &ResourcePermissions{}, Scopes: map[string]k8score.ResourceScope{}}
		rejection := apierrors.NewUnauthorized("credential rejected")
		if namespaceFailure {
			result.NamespaceProbeErrors = map[string]map[string]error{"pods": {"alpha": rejection}}
		} else {
			result.ProbeErrors = map[string]error{"pods": rejection}
		}
		calls := 0
		got, err := bootstrapResourcePermissions(context.Background(), func(context.Context, bool) (*PermissionCheckResult, bool) { calls++; return result, true })
		if got != nil || !apierrors.IsUnauthorized(err) || !isAuthClassification(ClassifyError(err)) || calls != 2 {
			t.Fatalf("namespace=%v result=%+v err=%v classification=%q calls=%d", namespaceFailure, got, err, ClassifyError(err), calls)
		}
	}
}

func TestCachePermissionEvidenceSurvivesIndependentGlobalRefresh(t *testing.T) {
	bound := &PermissionCheckResult{Perms: &ResourcePermissions{Pods: true}, Scopes: map[string]k8score.ResourceScope{"pods": {Enabled: true, Namespace: "alpha"}}, ScopeNamespaces: map[string][]string{"pods": {"alpha", "beta"}}}
	cache := &ResourceCache{permissionResult: clonePermissionCheckResult(bound)}
	restore := SetTestPermissionResult(&PermissionCheckResult{Perms: &ResourcePermissions{}, ProbeErrors: map[string]error{"pods": apierrors.NewUnauthorized("another generation")}})
	defer restore()
	evidence := cache.PermissionResult()
	if evidence.Scopes["pods"].Namespace != "alpha" || evidence.ProbeErrors["pods"] != nil {
		t.Fatalf("independent global evidence leaked into captured cache: %+v", evidence)
	}
	evidence.ScopeNamespaces["pods"][0] = "mutated"
	if cache.PermissionResult().ScopeNamespaces["pods"][0] != "alpha" {
		t.Fatal("caller mutated cache-bound evidence")
	}
}

func TestBootstrapNamespace401RecoveryWiresRecoveredInformerCoverage(t *testing.T) {
	dyn := fakeDyn(t, func(_ schema.GroupVersionResource, ns string) bool { return ns == "alpha" || ns == "beta" }).(*dynamicfake.FakeDynamicClient)
	first := true
	dyn.PrependReactor("list", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() == "alpha" && first {
			first = false
			return true, nil, apierrors.NewUnauthorized("startup token expired")
		}
		return false, nil, nil
	})
	calls := 0
	recovered, err := bootstrapResourcePermissions(context.Background(), func(ctx context.Context, retry bool) (*PermissionCheckResult, bool) {
		calls++
		result, _ := probeResourceAccess(ctx, dyn, []string{"alpha", "beta"}, false)
		if calls == 1 && (!apierrors.IsUnauthorized(result.NamespaceProbeErrors["pods"]["alpha"]) || result.Scopes["pods"].Namespace != "beta") {
			t.Fatalf("failed to reproduce narrowed startup scope: %+v", result)
		}
		return result, true
	})
	if err != nil || calls != 2 {
		t.Fatalf("recovery err=%v calls=%d", err, calls)
	}
	core, err := k8score.NewResourceCache(k8score.CacheConfig{Client: kubernetesfake.NewClientset(), ResourceScopes: recovered.Scopes, ResourceScopeNamespaces: recovered.ScopeNamespaces})
	if err != nil {
		t.Fatal(err)
	}
	defer core.Stop()
	if assessment := core.AssessKindRead("pods", []string{"alpha", "beta"}); !assessment.Complete {
		t.Fatalf("recovered credentials still froze narrowed scope: %+v", assessment)
	}
}

func TestBootstrapProbeBudgetExpiryKeepsNonAuthGrants(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	grants := &PermissionCheckResult{Perms: &ResourcePermissions{Pods: true}, Scopes: map[string]k8score.ResourceScope{"pods": {Enabled: true, Namespace: "alpha"}}, ScopeCandidatesTruncated: true}
	calls := 0
	got, err := bootstrapResourcePermissions(ctx, func(context.Context, bool) (*PermissionCheckResult, bool) { calls++; return grants, true })
	if err != nil || got != grants || calls != 1 {
		t.Fatalf("probe budget discarded completed grants: got=%+v err=%v calls=%d", got, err, calls)
	}
}

func TestBootstrap401RetryBudgetExpiryDoesNotPublishFalseRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	rejected := &PermissionCheckResult{Perms: &ResourcePermissions{}, ProbeErrors: map[string]error{"pods": apierrors.NewUnauthorized("startup credentials rejected")}}
	calls := 0
	got, err := bootstrapResourcePermissions(ctx, func(context.Context, bool) (*PermissionCheckResult, bool) {
		calls++
		if calls == 1 {
			return rejected, true
		}
		<-ctx.Done()
		return &PermissionCheckResult{Perms: &ResourcePermissions{}, Scopes: map[string]k8score.ResourceScope{}}, true
	})
	if got != nil || !apierrors.IsUnauthorized(err) || calls != 2 {
		t.Fatalf("expired retry implied credentials recovered: got=%+v err=%v calls=%d", got, err, calls)
	}
}
