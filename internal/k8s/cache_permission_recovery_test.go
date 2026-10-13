package k8s

import (
	"context"
	"testing"

	"github.com/skyhook-io/radar/pkg/k8score"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
