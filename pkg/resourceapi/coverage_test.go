package resourceapi

import (
	"fmt"
	"slices"
	"testing"
)

func TestNamespacePreviewPreservesMembershipMeaning(t *testing.T) {
	names := make([]string, 1000)
	for i := range names {
		names[i] = fmt.Sprintf("team-%04d", 999-i)
	}
	original := slices.Clone(names)
	preview := NamespacePreview(names, MCPNamespacePreview)
	if preview.Total != 1000 || preview.Omitted != 992 || len(preview.Names) != 8 {
		t.Fatalf("preview = %+v", preview)
	}
	if !slices.Equal(names, original) {
		t.Fatal("preview mutated caller membership")
	}
	if got := NamespacePreview([]string{"beta", "alpha", "alpha"}, RESTNamespacePreview); got.Total != 2 || got.Omitted != 0 || !slices.Equal(got.Names, []string{"alpha", "beta"}) {
		t.Fatalf("set = %+v", got)
	}
}
