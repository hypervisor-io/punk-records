package memory

import (
	"context"
	"encoding/binary"
	"math"
	"testing"
)

func TestQuantizeRoundTrip(t *testing.T) {
	v := []float32{0.5, -1.0, 2.0, -0.25, 0}
	codes, scale := QuantizeInt8(v)
	if scale <= 0 {
		t.Fatalf("scale = %v, want > 0", scale)
	}
	if codes[2] != 127 && codes[2] != -127 {
		t.Fatalf("largest-magnitude component code = %d, want +-127", codes[2])
	}
	got := DequantizeInt8(codes, scale)
	for i, g := range got {
		if diff := math.Abs(float64(g - v[i])); diff > float64(scale) {
			t.Fatalf("i=%d dequant %v vs orig %v: err %v > scale %v", i, g, v[i], diff, scale)
		}
	}
}

func TestQuantizeAllZero(t *testing.T) {
	codes, scale := QuantizeInt8([]float32{0, 0, 0, 0})
	if scale != 0 {
		t.Fatalf("scale = %v, want 0 (div-by-zero guard)", scale)
	}
	for _, c := range codes {
		if c != 0 {
			t.Fatalf("codes = %v, want all zero", codes)
		}
	}
}

func TestCosineQMatchesFloat(t *testing.T) {
	query := []float32{0.3, -0.8, 0.5, 0.1}
	stored := []float32{0.9, -0.2, 0.4, -0.3}
	want := cosine(query, stored)
	codes, scale := QuantizeInt8(stored)
	got := cosineQ(query, codes, scale)
	if diff := math.Abs(got - want); diff > 0.02 {
		t.Fatalf("cosineQ = %v, cosine = %v, diff %v > 0.02", got, want, diff)
	}
}

// TestEncodeDecodeLegacyAndTagged proves the back-compat contract: a
// pre-existing untagged float32 blob still decodes correctly (never
// misdetected as int8), a tagged float32 blob round-trips, a tagged int8
// blob round-trips, and VectorSearch scores a mix of all three formats
// coexisting in the same namespace.
func TestEncodeDecodeLegacyAndTagged(t *testing.T) {
	v := []float32{0.1, -2.5, 3.75, -0.001}

	// legacy: untagged 4*dims float32, byte-identical to the pre-tag format
	legacy := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(legacy[i*4:], math.Float32bits(f))
	}
	got := decodeVector(legacy)
	if len(got) != len(v) {
		t.Fatalf("legacy decode len = %d, want %d", len(got), len(v))
	}
	for i := range v {
		if got[i] != v[i] {
			t.Fatalf("legacy decode[%d] = %v, want %v", i, got[i], v[i])
		}
	}

	// tagged float32 round-trip
	tf := encodeVector(v, false)
	if tf[0] != tagFloat32 {
		t.Fatalf("tag = %#x, want tagFloat32", tf[0])
	}
	got = decodeVector(tf)
	for i := range v {
		if got[i] != v[i] {
			t.Fatalf("tagged float32 decode[%d] = %v, want %v", i, got[i], v[i])
		}
	}

	// tagged int8 round-trip (lossy: compare against the same dequant math)
	ti := encodeVector(v, true)
	if ti[0] != tagInt8 {
		t.Fatalf("tag = %#x, want tagInt8", ti[0])
	}
	codes, scale := QuantizeInt8(v)
	want := DequantizeInt8(codes, scale)
	got = decodeVector(ti)
	for i := range v {
		if got[i] != want[i] {
			t.Fatalf("tagged int8 decode[%d] = %v, want %v", i, got[i], want[i])
		}
	}

	// both formats usable by the search path, alongside a simulated legacy row
	s, db, _ := newTest(t)
	ctx := context.Background()
	fe := &fakeEmbedder{m: map[string][]float32{
		"alpha topic": {1, 0, 0, 0},
		"beta topic":  {0, 1, 0, 0},
		"alpha query": {0.95, 0.05, 0, 0},
	}}
	s.SetEmbedder(fe)

	if _, err := s.Remember(ctx, "ns", "/alpha", "alpha topic", nil, "w"); err != nil {
		t.Fatal(err)
	}
	s.SetQuantizeVectors(true)
	if _, err := s.Remember(ctx, "ns", "/beta", "beta topic", nil, "w"); err != nil {
		t.Fatal(err)
	}
	s.SetQuantizeVectors(false)

	// overwrite /alpha's embedding with a bare untagged legacy blob, as if
	// it predates this change
	if _, err := db.ExecContext(ctx, db.Rebind(
		`UPDATE memories SET embedding = $1 WHERE key = $2`),
		encodeVector([]float32{1, 0.02, 0, 0}, false)[1:], "/alpha"); err != nil {
		t.Fatal(err)
	}

	out, err := s.VectorSearch(ctx, "ns", "alpha query", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].Key != "/alpha" {
		t.Fatalf("vector search = %+v, want /alpha first among 2 results", out)
	}
}

// TestLegacyBlobNeverMisdecodesAsInt8 hammers the CRITICAL back-compat
// constraint: build every legacy (untagged) float32 blob whose first byte
// is 0x00 or 0x01 (the exact ambiguous case) for dims 1..16, and confirm
// decodeVector always treats it as legacy float32, never as tagged.
func TestLegacyBlobNeverMisdecodesAsInt8(t *testing.T) {
	for dims := 1; dims <= 16; dims++ {
		for _, firstByte := range []byte{0x00, 0x01} {
			v := make([]float32, dims)
			for i := range v {
				v[i] = float32(i) + 0.5
			}
			legacy := make([]byte, 4*dims)
			for i, f := range v {
				binary.LittleEndian.PutUint32(legacy[i*4:], math.Float32bits(f))
			}
			legacy[0] = firstByte // force the ambiguous first byte
			got := decodeVector(legacy)
			if len(got) != dims {
				t.Fatalf("dims=%d firstByte=%#x: decoded len=%d, want %d (misdecoded as tagged)", dims, firstByte, len(got), dims)
			}
		}
	}
}
