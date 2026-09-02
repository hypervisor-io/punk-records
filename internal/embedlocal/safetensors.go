// Package embedlocal implements memory.Embedder with a static
// token-embedding table loaded from a safetensors file: no model
// runtime, one file, deterministic vectors.
package embedlocal

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
)

type tensorHeader struct {
	Dtype       string `json:"dtype"`
	Shape       []int  `json:"shape"`
	DataOffsets [2]int `json:"data_offsets"`
}

// ReadMatrix loads a rank-2 F16 or F32 tensor from a safetensors file
// into a row-major float32 slice.
func ReadMatrix(path, tensor string) (rows, dims int, data []float32, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, nil, err
	}
	if len(raw) < 8 {
		return 0, 0, nil, fmt.Errorf("safetensors: file too short")
	}
	n := binary.LittleEndian.Uint64(raw[:8])
	if n > uint64(len(raw)-8) {
		return 0, 0, nil, fmt.Errorf("safetensors: header length %d exceeds file", n)
	}
	var header map[string]json.RawMessage
	if err := json.Unmarshal(raw[8:8+n], &header); err != nil {
		return 0, 0, nil, fmt.Errorf("safetensors: header: %w", err)
	}
	th, ok := header[tensor]
	if !ok {
		return 0, 0, nil, fmt.Errorf("safetensors: tensor %q not found", tensor)
	}
	var h tensorHeader
	if err := json.Unmarshal(th, &h); err != nil {
		return 0, 0, nil, fmt.Errorf("safetensors: tensor %q: %w", tensor, err)
	}
	if len(h.Shape) != 2 {
		return 0, 0, nil, fmt.Errorf("safetensors: tensor %q has rank %d, want 2", tensor, len(h.Shape))
	}
	rows, dims = h.Shape[0], h.Shape[1]
	if rows <= 0 || dims <= 0 || rows > math.MaxInt/dims {
		return 0, 0, nil, fmt.Errorf("safetensors: tensor %q shape %dx%d is not a usable matrix", tensor, rows, dims)
	}
	body := raw[8+n:]
	start, end := h.DataOffsets[0], h.DataOffsets[1]
	if start < 0 || end > len(body) || start > end {
		return 0, 0, nil, fmt.Errorf("safetensors: tensor %q offsets out of range", tensor)
	}
	seg := body[start:end]
	count := rows * dims
	data = make([]float32, count)
	switch h.Dtype {
	case "F16":
		if len(seg) != count*2 {
			return 0, 0, nil, fmt.Errorf("safetensors: F16 byte length %d != %d", len(seg), count*2)
		}
		for i := 0; i < count; i++ {
			data[i] = f16to32(binary.LittleEndian.Uint16(seg[2*i:]))
		}
	case "F32":
		if len(seg) != count*4 {
			return 0, 0, nil, fmt.Errorf("safetensors: F32 byte length %d != %d", len(seg), count*4)
		}
		for i := 0; i < count; i++ {
			data[i] = math.Float32frombits(binary.LittleEndian.Uint32(seg[4*i:]))
		}
	default:
		return 0, 0, nil, fmt.Errorf("safetensors: unsupported dtype %q", h.Dtype)
	}
	return rows, dims, data, nil
}

// f16to32 decodes an IEEE 754 binary16 value.
func f16to32(h uint16) float32 {
	sign := uint32(h>>15) << 31
	exp := int((h >> 10) & 0x1F)
	mant := uint32(h & 0x3FF)
	switch exp {
	case 0:
		if mant == 0 {
			return math.Float32frombits(sign)
		}
		// subnormal: value = mant * 2^-24
		return math.Float32frombits(sign) + float32(mant)*float32(5.9604645e-08)*sgn(sign)
	case 0x1F:
		if mant == 0 {
			return math.Float32frombits(sign | 0x7F800000)
		}
		return math.Float32frombits(sign | 0x7FC00000)
	}
	return math.Float32frombits(sign | uint32(exp+112)<<23 | mant<<13)
}

func sgn(signBit uint32) float32 {
	if signBit != 0 {
		return -1
	}
	return 1
}
