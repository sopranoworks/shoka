package index

import (
	"reflect"
	"testing"
)

// OutboundLinks rides the record like Bigrams: additive, no migration.
func TestOutboundLinksRoundTrip(t *testing.T) {
	idx, err := Create(tmpIndexPath(t), "ns", "proj")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer idx.Close()

	want := IndexRecord{Etag: "e1", OutboundLinks: []string{"a.md", "docs/b.md"}}
	if err := idx.PutRecord("ref.md", want); err != nil {
		t.Fatalf("PutRecord: %v", err)
	}
	got, found, err := idx.GetRecord("ref.md")
	if err != nil || !found {
		t.Fatalf("GetRecord: found=%v err=%v", found, err)
	}
	if !reflect.DeepEqual(got.OutboundLinks, want.OutboundLinks) {
		t.Errorf("OutboundLinks = %q, want %q", got.OutboundLinks, want.OutboundLinks)
	}
}

// Referrers inverts the forward outbound-link map: "who links to P" is every
// record whose OutboundLinks contains P, returned sorted.
func TestReferrers(t *testing.T) {
	idx, err := Create(tmpIndexPath(t), "ns", "proj")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer idx.Close()

	put := func(path string, outbound ...string) {
		if err := idx.PutRecord(path, IndexRecord{Etag: "x", OutboundLinks: outbound}); err != nil {
			t.Fatalf("PutRecord %s: %v", path, err)
		}
	}
	put("a.md", "target.md", "other.md")
	put("docs/b.md", "target.md")
	put("c.md", "unrelated.md")
	put("d.md") // no outbound links

	refs, err := idx.Referrers("target.md")
	if err != nil {
		t.Fatalf("Referrers: %v", err)
	}
	want := []string{"a.md", "docs/b.md"}
	if !reflect.DeepEqual(refs, want) {
		t.Errorf("Referrers(target.md) = %q, want %q", refs, want)
	}

	none, err := idx.Referrers("nobody.md")
	if err != nil {
		t.Fatalf("Referrers(nobody): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("Referrers(nobody.md) = %q, want empty", none)
	}
}

// Referrers normalises its query the same way records are keyed/stored, so a
// leading slash or "./" prefix on the queried target resolves identically.
func TestReferrersNormalisesQuery(t *testing.T) {
	idx, err := Create(tmpIndexPath(t), "ns", "proj")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer idx.Close()
	if err := idx.PutRecord("a.md", IndexRecord{Etag: "x", OutboundLinks: []string{"sub/t.md"}}); err != nil {
		t.Fatalf("PutRecord: %v", err)
	}
	for _, q := range []string{"sub/t.md", "/sub/t.md", "./sub/t.md"} {
		refs, err := idx.Referrers(q)
		if err != nil {
			t.Fatalf("Referrers(%q): %v", q, err)
		}
		if !reflect.DeepEqual(refs, []string{"a.md"}) {
			t.Errorf("Referrers(%q) = %q, want [a.md]", q, refs)
		}
	}
}
