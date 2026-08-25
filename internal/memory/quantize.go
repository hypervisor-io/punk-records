package memory

import "math"

// QuantizeInt8 maps a float32 vector to int8 via per-vector symmetric
// scaling: scale = max(|v_i|)/127; q_i = round(v_i/scale). The
// largest-magnitude component always lands on +-127. An all-zero vector
// gets scale 0 and all-zero codes (avoids a div-by-zero, not a real vector
// anyone will search for).
func QuantizeInt8(v []float32) (codes []int8, scale float32) {
	var maxAbs float32
	for _, f := range v {
		if a := float32(math.Abs(float64(f))); a > maxAbs {
			maxAbs = a
		}
	}
	codes = make([]int8, len(v))
	if maxAbs == 0 {
		return codes, 0
	}
	scale = maxAbs / 127
	for i, f := range v {
		q := math.Round(float64(f / scale))
		switch {
		case q > 127:
			q = 127
		case q < -127:
			q = -127
		}
		codes[i] = int8(q)
	}
	return codes, scale
}

// DequantizeInt8 reconstructs an approximate float32 vector.
func DequantizeInt8(codes []int8, scale float32) []float32 {
	v := make([]float32, len(codes))
	for i, c := range codes {
		v[i] = float32(c) * scale
	}
	return v
}

// cosineQ scores a float32 query against an int8-quantized stored vector,
// without materializing the dequantized float32 vector: integer dot
// product plus norms in codes-space. Cosine similarity is scale-invariant
// (cosine(a, k*b) == cosine(a, b) for k > 0), so the scale cancels
// algebraically and never needs to be multiplied through -- this is
// exactly cosine(query, DequantizeInt8(codes, scale)), just without the
// allocation.
//
// ponytail: per-vector scalar quantization only buys 4x over float32. If
// that's not enough, the next rung is product quantization (sub-vector
// codebooks, 8-16x) -- not a hand-tuned variant of this.
func cosineQ(query []float32, codes []int8, scale float32) float64 {
	if len(query) != len(codes) || len(query) == 0 || scale == 0 {
		return -1
	}
	var dot, nq, nc float64
	for i, q := range query {
		c := float64(codes[i])
		dot += float64(q) * c
		nq += float64(q) * float64(q)
		nc += c * c
	}
	if nq == 0 || nc == 0 {
		return -1
	}
	return dot / (math.Sqrt(nq) * math.Sqrt(nc))
}
