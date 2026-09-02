package embedlocal

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync/atomic"
)

// Static embeds text as the L2-normalised mean of static token vectors.
// Deterministic, allocation-light, no model runtime.
type Static struct {
	tok       *WordPiece
	table     []float32 // rows*dims, row-major
	rows      int
	dims      int
	normalize bool
	maxTokens int
	truncated atomic.Int64
}

type modelConfig struct {
	Normalize bool `json:"normalize"`
}

// Load reads config.json, tokenizer.json and model.safetensors from dir.
// maxTokens <= 0 means 1024.
func Load(dir string, maxTokens int) (*Static, error) {
	if maxTokens <= 0 {
		maxTokens = 1024
	}
	cfgRaw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return nil, err
	}
	var cfg modelConfig
	if err := json.Unmarshal(cfgRaw, &cfg); err != nil {
		return nil, fmt.Errorf("config.json: %w", err)
	}
	tokRaw, err := os.ReadFile(filepath.Join(dir, "tokenizer.json"))
	if err != nil {
		return nil, err
	}
	tok, err := LoadWordPiece(tokRaw)
	if err != nil {
		return nil, err
	}
	rows, dims, table, err := ReadMatrix(filepath.Join(dir, "model.safetensors"), "embeddings")
	if err != nil {
		return nil, err
	}
	return &Static{tok: tok, table: table, rows: rows, dims: dims, normalize: cfg.Normalize, maxTokens: maxTokens}, nil
}

// Dims is the vector width.
func (s *Static) Dims() int { return s.dims }

// Truncated counts inputs whose token list exceeded maxTokens since load.
func (s *Static) Truncated() int64 { return s.truncated.Load() }

// Embed returns one vector per text. Unknown tokens are dropped before
// pooling; a text with no known tokens yields the zero vector.
func (s *Static) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ids := s.tok.Encode(text)
		if len(ids) > s.maxTokens {
			ids = ids[:s.maxTokens]
			s.truncated.Add(1)
		}
		vec := make([]float32, s.dims)
		n := 0
		for _, id := range ids {
			if id == s.tok.UnkID() || id < 0 || id >= s.rows {
				continue
			}
			row := s.table[id*s.dims : (id+1)*s.dims]
			for d, v := range row {
				vec[d] += v
			}
			n++
		}
		if n > 0 {
			inv := 1 / float32(n)
			for d := range vec {
				vec[d] *= inv
			}
			if s.normalize {
				var sum float64
				for _, v := range vec {
					sum += float64(v) * float64(v)
				}
				if sum > 0 {
					scale := float32(1 / math.Sqrt(sum))
					for d := range vec {
						vec[d] *= scale
					}
				}
			}
		}
		out[i] = vec
	}
	return out, nil
}
