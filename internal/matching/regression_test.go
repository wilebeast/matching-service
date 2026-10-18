package matching

import (
	"encoding/json"
	"testing"
)

func TestReviewNegativeZero(t *testing.T) {
	c := fixture(t)
	c.Strategies = c.Strategies[:1]
	c.Candidates = c.Candidates[:1]
	var minuszero any
	json.Unmarshal([]byte("-0"), &minuszero)
	c.Candidates[0].Attributes["salary"] = minuszero
	c.Strategies[0].Query = Rule{Field: "candidate.salary", Op: "eq", Value: lit(0)}
	e, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.Match(request())
	if err != nil {
		t.Fatal(err)
	}
	if r.Total != 1 {
		t.Fatalf("numeric -0 == 0 should match, got %d", r.Total)
	}
}
func TestReviewSurrogateLiteral(t *testing.T) {
	c := fixture(t)
	c.ContextSchema.Fields["user.name"] = Field{Type: "keyword"}
	c.Strategies = c.Strategies[:1]
	c.Strategies[0].When = Rule{Field: "user.name", Op: "eq", Value: &Value{Literal: json.RawMessage(`"\ud83d\ude00"`)}}
	_, err := New(c)
	if err != nil {
		t.Fatalf("valid JSON emoji string rejected: %v", err)
	}
}
func TestReviewEscapedSlash(t *testing.T) {
	c := fixture(t)
	c.ContextSchema.Fields["user.name"] = Field{Type: "keyword"}
	c.Strategies = c.Strategies[:1]
	c.Strategies[0].When = Rule{Field: "user.name", Op: "eq", Value: &Value{Literal: json.RawMessage(`"a\/b"`)}}
	_, err := New(c)
	if err != nil {
		t.Fatalf("valid JSON escaped slash rejected: %v", err)
	}
}

func TestJSONLiteralsEvaluateAsData(t *testing.T) {
	for _, tc := range []struct{ name, raw, value string }{
		{"emoji", `"\ud83d\ude00"`, "😀"},
		{"slash", `"a\/b"`, "a/b"},
		{"quotes", `"quote\" and newline\n"`, "quote\" and newline\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fixture(t)
			c.ContextSchema.Fields["user.name"] = Field{Type: "keyword"}
			c.Strategies = c.Strategies[:1]
			c.Strategies[0].When = Rule{Field: "user.name", Op: "eq", Value: &Value{Literal: json.RawMessage(tc.raw)}}
			e, err := New(c)
			if err != nil {
				t.Fatal(err)
			}
			req := request()
			req.Context["user"].(map[string]any)["name"] = tc.value
			r, err := e.Match(req)
			if err != nil || r.Total != 1 {
				t.Fatalf("match %+v err=%v", r, err)
			}
			req.Context["user"].(map[string]any)["name"] = "different"
			r, err = e.Match(req)
			if err != nil || r.Total != 0 {
				t.Fatalf("mismatch %+v err=%v", r, err)
			}
		})
	}
}
func TestNegativeZeroNotEqualDoesNotMatch(t *testing.T) {
	c := fixture(t)
	c.Strategies = c.Strategies[:1]
	c.Candidates = c.Candidates[:1]
	var zero any
	if err := json.Unmarshal([]byte("-0"), &zero); err != nil {
		t.Fatal(err)
	}
	c.Candidates[0].Attributes["salary"] = zero
	c.Strategies[0].Query = Rule{Field: "candidate.salary", Op: "ne", Value: lit(0)}
	e, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.Match(request())
	if err != nil || r.Total != 0 {
		t.Fatalf("negative zero ne zero: %+v %v", r, err)
	}
}
