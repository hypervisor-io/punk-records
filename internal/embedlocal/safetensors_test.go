package embedlocal

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// writeSafetensors builds a minimal file: 8-byte LE header length, JSON
// header, raw tensor bytes.
func writeSafetensors(t *testing.T, dir, name, dtype string, shape []int, raw []byte) string {
	t.Helper()
	hdr := map[string]any{
		name: map[string]any{"dtype": dtype, "shape": shape, "data_offsets": []int{0, len(raw)}},
	}
	hj, _ := json.Marshal(hdr)
	var buf []byte
	buf = binary.LittleEndian.AppendUint64(buf, uint64(len(hj)))
	buf = append(buf, hj...)
	buf = append(buf, raw...)
	p := filepath.Join(dir, "model.safetensors")
	if err := os.WriteFile(p, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestF16To32(t *testing.T) {
	cases := map[uint16]float32{
		0x3C00: 1, 0xC000: -2, 0x0000: 0, 0x3555: 0.33325195, 0x7BFF: 65504, 0x0001: 5.9604645e-08,
	}
	for h, want := range cases {
		if got := f16to32(h); got != want {
			t.Fatalf("f16to32(%#04x) = %v, want %v", h, got, want)
		}
	}
	if !math.IsInf(float64(f16to32(0x7C00)), 1) || !math.IsNaN(float64(f16to32(0x7E00))) {
		t.Fatalf("inf/nan not decoded")
	}
}

func TestReadMatrixF16AndF32(t *testing.T) {
	dir := t.TempDir()
	raw16 := []byte{0x00, 0x3C, 0x00, 0xC0, 0x00, 0x00, 0x00, 0x3C, 0x00, 0x3C, 0x00, 0xC0} // [[1,-2,0],[1,1,-2]]
	p := writeSafetensors(t, dir, "embeddings", "F16", []int{2, 3}, raw16)
	rows, dims, data, err := ReadMatrix(p, "embeddings")
	if err != nil {
		t.Fatal(err)
	}
	if rows != 2 || dims != 3 {
		t.Fatalf("shape = %dx%d", rows, dims)
	}
	want := []float32{1, -2, 0, 1, 1, -2}
	for i := range want {
		if data[i] != want[i] {
			t.Fatalf("data = %v, want %v", data, want)
		}
	}
	raw32 := make([]byte, 0, 8)
	raw32 = binary.LittleEndian.AppendUint32(raw32, math.Float32bits(0.5))
	raw32 = binary.LittleEndian.AppendUint32(raw32, math.Float32bits(-0.25))
	p = writeSafetensors(t, t.TempDir(), "embeddings", "F32", []int{1, 2}, raw32)
	_, _, data, err = ReadMatrix(p, "embeddings")
	if err != nil || data[0] != 0.5 || data[1] != -0.25 {
		t.Fatalf("f32: %v %v", data, err)
	}
	if _, _, _, err := ReadMatrix(p, "missing"); err == nil {
		t.Fatalf("missing tensor must error")
	}
}

func TestReadMatrixRejectsOverflowingShape(t *testing.T) {
	dir := t.TempDir()
	p := writeSafetensors(t, dir, "embeddings", "F16", []int{1 << 40, 1 << 40}, []byte{0, 0})
	if _, _, _, err := ReadMatrix(p, "embeddings"); err == nil {
		t.Fatal("shape whose product overflows int must be rejected")
	}
	p = writeSafetensors(t, t.TempDir(), "embeddings", "F16", []int{0, 4}, nil)
	if _, _, _, err := ReadMatrix(p, "embeddings"); err == nil {
		t.Fatal("zero rows must be rejected")
	}
}
