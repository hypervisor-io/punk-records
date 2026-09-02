package embedlocal

import (
	"context"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func f16(v float32) []byte {
	// Only exact small values are used here: 0, 1, -1, 0.5.
	var h uint16
	switch v {
	case 0:
		h = 0
	case 1:
		h = 0x3C00
	case -1:
		h = 0xBC00
	case 0.5:
		h = 0x3800
	default:
		panic("unsupported test value")
	}
	return binary.LittleEndian.AppendUint16(nil, h)
}

func writeModelDir(t *testing.T, normalize bool) string {
	t.Helper()
	dir := t.TempDir()
	// vocab ids: [PAD]0 [UNK]1 hello2 world3 ; rows are 2-dim.
	rows := [][]float32{{0, 0}, {0, 0}, {1, 0}, {0, 1}}
	var raw []byte
	for _, r := range rows {
		for _, v := range r {
			raw = append(raw, f16(v)...)
		}
	}
	writeSafetensors(t, dir, "embeddings", "F16", []int{4, 2}, raw)
	tok := `{"normalizer":{"type":"BertNormalizer","lowercase":true,"handle_chinese_chars":true},"pre_tokenizer":{"type":"BertPreTokenizer"},
	  "model":{"type":"WordPiece","unk_token":"[UNK]","continuing_subword_prefix":"##","max_input_chars_per_word":100,"vocab":{"[PAD]":0,"[UNK]":1,"hello":2,"world":3}}}`
	if err := os.WriteFile(filepath.Join(dir, "tokenizer.json"), []byte(tok), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := `{"normalize": false, "embedding_dtype": "float16"}`
	if normalize {
		cfg = `{"normalize": true, "embedding_dtype": "float16"}`
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestStaticEmbedMeanPoolAndNormalize(t *testing.T) {
	s, err := Load(writeModelDir(t, true), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if s.Dims() != 2 {
		t.Fatalf("dims = %d", s.Dims())
	}
	vecs, err := s.Embed(context.Background(), []string{"Hello world", "hello", "zzz", ""})
	if err != nil {
		t.Fatal(err)
	}
	inv := float32(1 / math.Sqrt2)
	if math.Abs(float64(vecs[0][0]-inv)) > 1e-6 || math.Abs(float64(vecs[0][1]-inv)) > 1e-6 {
		t.Fatalf("mean of (1,0),(0,1) normalised = %v", vecs[0])
	}
	if vecs[1][0] != 1 || vecs[1][1] != 0 {
		t.Fatalf("single token = %v", vecs[1])
	}
	if vecs[2][0] != 0 || vecs[2][1] != 0 {
		t.Fatalf("unknown-only text must be a zero vector, got %v", vecs[2])
	}
	if vecs[3][0] != 0 || vecs[3][1] != 0 {
		t.Fatalf("empty text must be a zero vector, got %v", vecs[3])
	}
}

func TestStaticEmbedNoNormalizeAndTruncation(t *testing.T) {
	s, err := Load(writeModelDir(t, false), 1)
	if err != nil {
		t.Fatal(err)
	}
	vecs, err := s.Embed(context.Background(), []string{"hello world"})
	if err != nil {
		t.Fatal(err)
	}
	if vecs[0][0] != 1 || vecs[0][1] != 0 {
		t.Fatalf("maxTokens=1 must keep only 'hello': %v", vecs[0])
	}
	if s.Truncated() != 1 {
		t.Fatalf("truncated = %d, want 1", s.Truncated())
	}
}
