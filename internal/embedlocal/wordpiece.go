package embedlocal

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// WordPiece is a BERT-style tokenizer: normalise, split on whitespace and
// punctuation, then greedy longest-match subwords with a "##" prefix for
// continuations. Only what the static embedding models need.
type WordPiece struct {
	vocab        map[string]int
	unkID        int
	prefix       string
	maxWordChars int
	lowercase    bool
	cjk          bool
}

type tokenizerFile struct {
	Normalizer struct {
		Type               string `json:"type"`
		Lowercase          bool   `json:"lowercase"`
		HandleChineseChars bool   `json:"handle_chinese_chars"`
	} `json:"normalizer"`
	Model struct {
		Type                    string         `json:"type"`
		UnkToken                string         `json:"unk_token"`
		ContinuingSubwordPrefix string         `json:"continuing_subword_prefix"`
		MaxInputCharsPerWord    int            `json:"max_input_chars_per_word"`
		Vocab                   map[string]int `json:"vocab"`
	} `json:"model"`
}

// LoadWordPiece parses a tokenizer.json in the HF tokenizers format.
func LoadWordPiece(tokenizerJSON []byte) (*WordPiece, error) {
	var f tokenizerFile
	if err := json.Unmarshal(tokenizerJSON, &f); err != nil {
		return nil, fmt.Errorf("tokenizer.json: %w", err)
	}
	if f.Model.Type != "WordPiece" {
		return nil, fmt.Errorf("tokenizer.json: model type %q not supported (want WordPiece)", f.Model.Type)
	}
	unk, ok := f.Model.Vocab[f.Model.UnkToken]
	if !ok {
		return nil, fmt.Errorf("tokenizer.json: unk token %q not in vocab", f.Model.UnkToken)
	}
	w := &WordPiece{
		vocab:        f.Model.Vocab,
		unkID:        unk,
		prefix:       f.Model.ContinuingSubwordPrefix,
		maxWordChars: f.Model.MaxInputCharsPerWord,
		lowercase:    f.Normalizer.Type == "" || f.Normalizer.Lowercase,
		cjk:          f.Normalizer.Type == "" || f.Normalizer.HandleChineseChars,
	}
	if w.prefix == "" {
		w.prefix = "##"
	}
	if w.maxWordChars <= 0 {
		w.maxWordChars = 100
	}
	return w, nil
}

// UnkID is the id of the unknown token; callers drop it before pooling.
func (w *WordPiece) UnkID() int { return w.unkID }

func isCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) || (r >= 0x3400 && r <= 0x4DBF) ||
		(r >= 0x20000 && r <= 0x2A6DF) || (r >= 0x2A700 && r <= 0x2B73F) ||
		(r >= 0x2B740 && r <= 0x2B81F) || (r >= 0x2B820 && r <= 0x2CEAF) ||
		(r >= 0xF900 && r <= 0xFAFF) || (r >= 0x2F800 && r <= 0x2FA1F)
}

// isPunct mirrors BERT: ASCII symbol ranges plus any Unicode P category.
func isPunct(r rune) bool {
	if (r >= 33 && r <= 47) || (r >= 58 && r <= 64) || (r >= 91 && r <= 96) || (r >= 123 && r <= 126) {
		return true
	}
	return unicode.IsPunct(r)
}

// normalize applies BertNormalizer: drop control characters and U+FFFD,
// map whitespace to space, isolate CJK characters, lowercase, strip
// combining marks (accents) via NFD.
func (w *WordPiece) normalize(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		switch {
		case r == 0 || r == 0xFFFD || (unicode.IsControl(r) && r != '\t' && r != '\n' && r != '\r'):
			continue
		case unicode.IsSpace(r):
			b.WriteByte(' ')
		case w.cjk && isCJK(r):
			b.WriteByte(' ')
			b.WriteRune(r)
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	s := b.String()
	if w.lowercase {
		s = strings.ToLower(s)
		// BERT strips accents whenever it lowercases (strip_accents: null).
		var out strings.Builder
		for _, r := range norm.NFD.String(s) {
			if unicode.Is(unicode.Mn, r) {
				continue
			}
			out.WriteRune(r)
		}
		s = out.String()
	}
	return s
}

// preTokenize splits on whitespace and makes every punctuation rune its
// own token.
func preTokenize(s string) []string {
	var words []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			words = append(words, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r == ' ':
			flush()
		case isPunct(r):
			flush()
			words = append(words, string(r))
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return words
}

// Encode returns token ids for text; unknown words map to UnkID.
func (w *WordPiece) Encode(text string) []int {
	var ids []int
	for _, word := range preTokenize(w.normalize(text)) {
		runes := []rune(word)
		if len(runes) > w.maxWordChars {
			ids = append(ids, w.unkID)
			continue
		}
		start := 0
		var sub []int
		bad := false
		for start < len(runes) {
			end := len(runes)
			found := -1
			for end > start {
				piece := string(runes[start:end])
				if start > 0 {
					piece = w.prefix + piece
				}
				if id, ok := w.vocab[piece]; ok {
					found = id
					break
				}
				end--
			}
			if found < 0 {
				bad = true
				break
			}
			sub = append(sub, found)
			start = end
		}
		if bad {
			ids = append(ids, w.unkID)
		} else {
			ids = append(ids, sub...)
		}
	}
	return ids
}
