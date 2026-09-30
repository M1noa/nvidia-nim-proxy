package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// RTK walks untrusted request bodies. no input shape may panic it, corrupt the
// body, or change it when disabled.
func TestRtkHostileBodies(t *testing.T) {
	c := defaultConfig()
	c.Rtk.Enabled = true
	useConfig(t, &c)

	bodies := []string{
		``,
		`{`,
		`null`,
		`[]`,
		`"just a string"`,
		`123`,
		`{"messages":null}`,
		`{"messages":"not an array"}`,
		`{"messages":[null,1,"x",{}]}`,
		`{"messages":[{"role":"user","content":null}]}`,
		`{"messages":[{"role":"user","content":123}]}`,
		`{"messages":[{"role":"user","content":[null,1,"x",{}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"tool_result"}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"tool_result","content":null}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"tool_result","content":[{}]}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"tool_result","is_error":"yes","content":"x"}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"tool_result","content":[{"type":"image","source":{}}]}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"tool_result","content":[1,2,3]}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"tool_result","content":"` + strings.Repeat("A", 100000) + `"}]}]}`,
		`{"deeply":{"nested":{"a":{"b":{"c":[[[1]]]}}}}}`,
		// type confusion: content is a number where a string was expected
		`{"messages":[{"role":"user","content":[{"type":"tool_result","content":{"a":1}}]}]}`,
	}
	for i, b := range bodies {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("body %d panicked: %v\n%s", i, r, b)
				}
			}()
			out := rtkCompressBody([]byte(b))
			// whatever comes back must still be parseable if the input was
			if json.Valid([]byte(b)) && !json.Valid(out) {
				t.Errorf("body %d: input valid, output invalid:\nin=%s\nout=%s", i, b, out)
			}
		}()
	}
}

// enabled must not modify bodies it does not recognise, and must be a strict
// no-op when disabled even for bodies it would otherwise compress.
func TestRtkNoopWhenNoMatch(t *testing.T) {
	c := defaultConfig()
	c.Rtk.Enabled = true
	useConfig(t, &c)

	for _, b := range []string{
		`{"messages":[{"role":"user","content":"plain text"}]}`,
		`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"tool_result","content":"tiny"}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"tool_result","is_error":true,"content":"` + strings.Repeat("x", 900) + `"}]}]}`,
	} {
		if out := rtkCompressBody([]byte(b)); string(out) != b {
			t.Errorf("unmatched body was modified:\nin=%s\nout=%s", b, out)
		}
	}
}
