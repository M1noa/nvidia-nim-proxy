package main

import (
	"net/http"

	"nimroute/pii"
)

// pii wiring: session guard from config + request headers, disclose line,
// and the streaming restore writer. replaces anon.go end to end.

// piiConfig converts the anonymize section to the pii package shape.
func piiConfig() pii.Config {
	c := cfg().Anonymize
	ents := make([]pii.CustomTerm, len(c.Entities))
	for i, e := range c.Entities {
		ents[i] = pii.CustomTerm{Name: e.Name, Type: e.Type, Variations: e.Variations, Replacement: e.Replacement}
	}
	return pii.Config{
		Enabled: true, Mode: c.Mode, Disclose: c.Disclose, DiscloseText: c.DiscloseText,
		Entities: ents, Terms: c.Terms, FuzzyThreshold: c.FuzzyThreshold,
		DetectSecrets: c.DetectSecrets, DetectPII: c.DetectPII,
		KeepLabels: c.KeepLabels, IncludeSystem: c.IncludeSystem,
		OnTimeout: c.OnTimeout, VaultTTLHours: c.VaultTTLHours, VaultMaxSize: c.VaultMaxSize,
		Ner: pii.NerConfig{Enabled: c.Ner.Enabled, ModelPath: c.Ner.ModelPath,
			OrtLib: c.Ner.OrtLib, TimeoutMs: c.Ner.TimeoutMs, MinScore: c.Ner.MinScore},
	}
}

// guardForRequest returns the session guard, or nil when disabled (zero
// overhead: callers pass bodies through untouched).
func guardForRequest(r *http.Request) *pii.Guard {
	if !cfg().Anonymize.Enabled {
		return nil
	}
	return pii.ForRequest(piiConfig(), piiSessionID(r))
}

func piiSessionID(r *http.Request) string {
	if s := r.Header.Get("X-Session-Id"); s != "" {
		return "sess:" + s
	}
	var got string
	if h := r.Header.Get("Authorization"); h != "" {
		got = h
	} else {
		got = r.Header.Get("x-api-key")
	}
	if got == "" {
		return "sess:default"
	}
	return pii.SessionKey(got)
}

// discloseLine returns the anonymize notice for system prompts, or "".
func discloseLine() string {
	return pii.DiscloseLine(piiConfigWithEnabled())
}

func piiConfigWithEnabled() pii.Config {
	c := piiConfig()
	c.Enabled = cfg().Anonymize.Enabled
	return c
}

// piiWriter restores surrogates in streaming response bytes, holding back
// only a suffix that could still be a partial surrogate.
type piiWriter struct {
	w http.ResponseWriter
	r *pii.Revealer
	g *pii.Guard
}

func wrapPII(w http.ResponseWriter, g *pii.Guard) *piiWriter {
	if g == nil {
		return nil
	}
	return &piiWriter{w: w, r: g.NewRevealer(), g: g}
}

func (d *piiWriter) Header() http.Header { return d.w.Header() }
func (d *piiWriter) WriteHeader(s int)   { d.w.WriteHeader(s) }

func (d *piiWriter) Write(b []byte) (int, error) {
	out := d.r.Push(b)
	if len(out) > 0 {
		if _, err := d.w.Write(out); err != nil {
			return 0, err
		}
	}
	return len(b), nil
}

func (d *piiWriter) Flush() {
	if f, ok := d.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (d *piiWriter) finish() {
	if d == nil {
		return
	}
	if tail := d.r.Flush(); len(tail) > 0 {
		d.w.Write(tail)
	}
}
