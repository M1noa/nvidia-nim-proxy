//go:build ner

package pii

import (
	"os"
	"testing"
)

func TestNerBackendLoads(t *testing.T) {
	model := os.Getenv("RAMPART_MODEL")
	lib := os.Getenv("ORT_LIB")
	if model == "" || lib == "" {
		t.Skip("RAMPART_MODEL or ORT_LIB unset")
	}
	d := NerDetector(model, lib, 2000, 0.4)
	if d == nil {
		t.Fatal("ner detector failed to load")
	}
	spans := d("My name is Alex Rivera and I live in Austin.")
	found := false
	for _, s := range spans {
		if (s.Label == LGivenName || s.Label == LCity) && s.Text != "" {
			found = true
		}
		t.Logf("ner span %q %s %.2f", s.Text, s.Label, s.Score)
	}
	if !found {
		t.Fatal("expected a name span from ner backend")
	}
}
