package crdt_test

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// V2 key dedup opt-in (follow-up to the yxml wire-conformance fix).
//
// The yjs reference encoder writes a fresh keyClock+string for EVERY
// occurrence of a key (dedup deliberately disabled upstream so older clients
// can read the bytes) — ygo matches that by default, byte-identically.
// Deployments that control both ends can opt in to key dedup for smaller V2
// updates: repeated keys (ContentFormat keys, XmlElement node names,
// parentSub) become back-references. Modern yjs readKey and ygo's decoder
// both accept the deduped shape.
//
// Fixture docs with genuinely repeated keys:
//   - nested_elements:   node names "list_item"/"paragraph" twice, plus attrs
//   - overlapping_marks: format keys "strong"/"em" twice each (open+close)

// dedupTestDoc reconstructs a fixture doc from its V1 bytes and returns the
// doc plus its expected canonical tree.
func dedupTestDoc(t *testing.T, name string) (*crdt.Doc, []any) {
	t.Helper()
	fx, ok := loadYxmlYjsFixtures(t)[name]
	if !ok {
		t.Fatalf("fixture %s missing", name)
	}
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, mustHex(t, fx.V1), nil); err != nil {
		t.Fatalf("seed decode: %v", err)
	}
	return doc, fx.Expected
}

// TestV2KeyDedup_DefaultOffIsConformant is the regression guard: with no
// option, EncodeStateAsUpdateV2 must stay byte-identical to the yjs
// reference — the opt-in must not disturb the default path.
func TestV2KeyDedup_DefaultOffIsConformant(t *testing.T) {
	for _, name := range []string{"nested_elements", "overlapping_marks"} {
		name := name
		t.Run(name, func(t *testing.T) {
			doc, _ := dedupTestDoc(t, name)
			fx := loadYxmlYjsFixtures(t)[name]
			if got := hex.EncodeToString(crdt.EncodeStateAsUpdateV2(doc, nil)); got != fx.V2 {
				t.Errorf("default (dedup OFF) drifted from yjs reference:\n  got:  %s\n  want: %s", got, fx.V2)
			}
			// The Opts variant with no options must be the same bytes too.
			if got := hex.EncodeToString(crdt.EncodeStateAsUpdateV2Opts(doc, nil)); got != fx.V2 {
				t.Errorf("Opts variant without options drifted:\n  got:  %s\n  want: %s", got, fx.V2)
			}
		})
	}
}

// TestV2KeyDedup_OptInShorterAndRoundTrips proves the dedup actually fires:
// the opt-in encoding must be STRICTLY shorter than the conformant encoding
// (repeated keys became back-references), and applying it must reconstruct
// the identical canonical tree through ygo's decoder (which, like modern
// yjs readKey, supports the deduped shape).
func TestV2KeyDedup_OptInShorterAndRoundTrips(t *testing.T) {
	for _, name := range []string{"nested_elements", "overlapping_marks"} {
		name := name
		t.Run(name, func(t *testing.T) {
			doc, expected := dedupTestDoc(t, name)
			off := crdt.EncodeStateAsUpdateV2(doc, nil)
			on := crdt.EncodeStateAsUpdateV2Opts(doc, nil, crdt.WithV2KeyDedup())

			if len(on) >= len(off) {
				t.Fatalf("dedup did not fire: ON %d bytes, OFF %d bytes", len(on), len(off))
			}
			if bytes.Equal(on, off) {
				t.Fatal("ON and OFF encodings are identical")
			}

			dst := crdt.New()
			if err := crdt.ApplyUpdateV2(dst, on, nil); err != nil {
				t.Fatalf("ApplyUpdateV2 of deduped bytes: %v", err)
			}
			assertCanonical(t, dst.GetXmlFragment("prosemirror"), expected)
		})
	}
}

// TestV2KeyDedup_ThreadsThroughDiffAndMerge covers the struct-level encoders
// (the doc-server send paths): DiffUpdateV2Opts and MergeUpdatesV2Opts must
// honour the option the same way, and their deduped output must round-trip.
//
// Uses a root-level YText with repeated format keys rather than the XML
// fixtures: the struct-level V2 DECODE path (buildMergeStore/decodeStructsV2,
// update_v2.go "parent item not found") predates this change and cannot yet
// resolve nested parent-by-ID references — issue #146 territory, addressed
// by upstream PR #145, deliberately not fixed here.
func TestV2KeyDedup_ThreadsThroughDiffAndMerge(t *testing.T) {
	doc := crdt.New(crdt.WithClientID(7))
	txt := doc.GetText("t")
	doc.Transact(func(txn *crdt.Transaction) {
		txt.Insert(txn, 0, "abcdef", nil)
		txt.Format(txn, 0, 2, crdt.Attributes{"bold": true})
		txt.Format(txn, 4, 2, crdt.Attributes{"bold": true}) // "bold" key recurs
	}, nil)
	off := crdt.EncodeStateAsUpdateV2(doc, nil)

	offDiff, err := crdt.DiffUpdateV2(off, crdt.StateVector{})
	if err != nil {
		t.Fatalf("DiffUpdateV2: %v", err)
	}
	onDiff, err := crdt.DiffUpdateV2Opts(off, crdt.StateVector{}, crdt.WithV2KeyDedup())
	if err != nil {
		t.Fatalf("DiffUpdateV2Opts: %v", err)
	}
	if len(onDiff) >= len(offDiff) {
		t.Fatalf("diff dedup did not fire: ON %d bytes, OFF %d bytes", len(onDiff), len(offDiff))
	}

	offMerge, err := crdt.MergeUpdatesV2(off)
	if err != nil {
		t.Fatalf("MergeUpdatesV2: %v", err)
	}
	onMerge, err := crdt.MergeUpdatesV2Opts([][]byte{off}, crdt.WithV2KeyDedup())
	if err != nil {
		t.Fatalf("MergeUpdatesV2Opts: %v", err)
	}
	if len(onMerge) >= len(offMerge) {
		t.Fatalf("merge dedup did not fire: ON %d bytes, OFF %d bytes", len(onMerge), len(offMerge))
	}

	// Deduped and conformant bytes must integrate to the same document:
	// integrate each and compare the (conformant) re-encodings byte-for-byte.
	want := hex.EncodeToString(off)
	for tag, enc := range map[string][]byte{"diff": onDiff, "merge": onMerge} {
		dst := crdt.New()
		if err := crdt.ApplyUpdateV2(dst, enc, nil); err != nil {
			t.Fatalf("ApplyUpdateV2 of deduped %s bytes: %v", tag, err)
		}
		if got := dst.GetText("t").ToString(); got != "abcdef" {
			t.Errorf("%s: text = %q, want %q", tag, got, "abcdef")
		}
		if got := hex.EncodeToString(crdt.EncodeStateAsUpdateV2(dst, nil)); got != want {
			t.Errorf("%s: re-encode after deduped apply drifted:\n  got:  %s\n  want: %s", tag, got, want)
		}
	}
}
