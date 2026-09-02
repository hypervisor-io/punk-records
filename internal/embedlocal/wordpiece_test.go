package embedlocal

import (
	"reflect"
	"testing"
)

const tinyTokenizer = `{
  "normalizer": {"type": "BertNormalizer", "clean_text": true, "handle_chinese_chars": true, "strip_accents": null, "lowercase": true},
  "pre_tokenizer": {"type": "BertPreTokenizer"},
  "model": {"type": "WordPiece", "unk_token": "[UNK]", "continuing_subword_prefix": "##", "max_input_chars_per_word": 100,
    "vocab": {"[PAD]": 0, "[UNK]": 1, ",": 2, "hello": 3, "world": 4, "un": 5, "##aff": 6, "##able": 7, "caf": 8, "##e": 9, "中": 10}}
}`

func TestWordPieceEncode(t *testing.T) {
	w, err := LoadWordPiece([]byte(tinyTokenizer))
	if err != nil {
		t.Fatal(err)
	}
	if w.UnkID() != 1 {
		t.Fatalf("unk id = %d", w.UnkID())
	}
	cases := []struct {
		in   string
		want []int
	}{
		{"Hello, World", []int{3, 2, 4}},
		{"unaffable", []int{5, 6, 7}},
		{"Café", []int{8, 9}},            // accent stripped: cafe -> caf ##e
		{"hello中world", []int{3, 10, 4}}, // CJK char isolated
		{"xyz", []int{1}},                // no prefix in vocab
		{"  hello\t\nworld ", []int{3, 4}},
		{"", nil},
	}
	for _, c := range cases {
		if got := w.Encode(c.in); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("Encode(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestWordPieceRejectsOtherModels(t *testing.T) {
	if _, err := LoadWordPiece([]byte(`{"model":{"type":"BPE","vocab":{}}}`)); err == nil {
		t.Fatalf("BPE must be rejected")
	}
}
