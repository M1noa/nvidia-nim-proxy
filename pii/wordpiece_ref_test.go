package pii

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestWordPieceMatchesReference(t *testing.T) {
	refPath := os.Getenv("TOK_REF")
	if refPath == "" {
		t.Skip("TOK_REF unset")
	}
	vocabPath := os.Getenv("RAMPART_VOCAB")
	if vocabPath == "" {
		t.Skip("RAMPART_VOCAB unset")
	}
	raw, err := os.ReadFile(refPath)
	if err != nil {
		t.Fatalf("read ref: %v", err)
	}
	var corpus []struct {
		Text    string   `json:"text"`
		Tokens  []string `json:"tokens"`
		Offsets [][2]int `json:"offsets"`
	}
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("parse ref: %v", err)
	}
	w, err := LoadWordPiece(vocabPath)
	if err != nil {
		t.Fatalf("load vocab: %v", err)
	}
	for _, c := range corpus {
		lowered := strings.ToLower(c.Text)
		folded, starts, ends := foldWithMap(lowered)
		got := w.Tokenize(folded)
		if len(got) != len(c.Tokens) {
			t.Errorf("%q: %d toks, want %d\n got %v\nwant %v", c.Text, len(got), len(c.Tokens), toks(got), c.Tokens)
			continue
		}
		for i, g := range got {
			if g.Text != c.Tokens[i] {
				t.Errorf("%q tok %d: got %q want %q", c.Text, i, g.Text, c.Tokens[i])
				continue
			}
			// [UNK] has no faithful raw projection; ids carry it.
			if g.Text == "[UNK]" {
				if g.ID != w.unk {
					t.Errorf("%q tok %d: unk id = %d, want %d", c.Text, i, g.ID, w.unk)
				}
				continue
			}
			// project folded offsets back to raw: sliced raw must
			// fold to the token piece (modulo ## prefix).
			rs, re := starts[g.Start], ends[g.End-1]
			rawSlice := c.Text[rs:re]
			rf, _, _ := foldWithMap(strings.ToLower(rawSlice))
			want := strings.TrimPrefix(g.Text, "##")
			if rf != want {
				t.Errorf("%q tok %d: raw slice %q folds to %q, want %q", c.Text, i, rawSlice, rf, want)
			}
		}
	}
}

func toks(ts []Tok) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Text
	}
	return out
}
