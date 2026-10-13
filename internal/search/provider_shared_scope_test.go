package search

import (
	"github.com/skyhook-io/radar/pkg/readreason"
	"testing"
)

func TestNamedUnwatchedKindReportsCollectionGap(t *testing.T) {
	named := newCoverage(Query{KindFilter: []string{"Secret"}})
	named.kind("Secret", "", readreason.KindNotWatched)
	if got := named.result(); len(got) != 1 || got[0].Reason != readreason.KindNotWatched {
		t.Fatalf("named unavailable kind disappeared: %+v", got)
	}
	broad := newCoverage(Query{})
	broad.kind("Secret", "", readreason.KindNotWatched)
	if got := broad.result(); len(got) != 0 {
		t.Fatalf("broad query was flooded with unwatched kinds: %+v", got)
	}
}
