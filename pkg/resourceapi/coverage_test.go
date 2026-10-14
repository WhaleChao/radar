package resourceapi

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
)

func TestNamesKeepsCountAndSortedSample(t *testing.T) {
	names := make([]string, 1000)
	for i := range names {
		names[i] = fmt.Sprintf("team-%04d", 999-i)
	}
	original := slices.Clone(names)
	set := Names(names, MCPSample)
	if set.Count != 1000 || len(set.Sample) != 8 || set.Sample[0] != "team-0000" {
		t.Fatalf("set = %+v", set)
	}
	if !slices.Equal(names, original) {
		t.Fatal("Names mutated the caller's slice")
	}
	if got := Names([]string{"beta", "alpha", "alpha"}, RESTSample); got.Count != 2 || !slices.Equal(got.Sample, []string{"alpha", "beta"}) {
		t.Fatalf("set = %+v", got)
	}
}

// An empty set must say so: sample [] and count 0, never null.
func TestEmptySetsMarshalAsEmpty(t *testing.T) {
	for _, v := range []any{Names(nil, 8), Kinds(nil, 8), Objects(nil, 0, 8)} {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != `{"count":0,"sample":[]}` {
			t.Fatalf("%T = %s", v, raw)
		}
	}
}

func TestObjectsCountsBeyondItsSample(t *testing.T) {
	set := Objects([]ObjectRef{{Kind: "Pod", Namespace: "a", Name: "x"}}, 37, 8)
	if set.Count != 37 || len(set.Sample) != 1 {
		t.Fatalf("set = %+v", set)
	}
}

func TestKindsDeduplicatesByKindAndGroup(t *testing.T) {
	set := Kinds([]KindRef{{Kind: "Service", Group: "serving.knative.dev"}, {Kind: "Service"}, {Kind: "Service"}}, 8)
	if set.Count != 2 || set.Sample[0] != (KindRef{Kind: "Service"}) {
		t.Fatalf("set = %+v", set)
	}
}
