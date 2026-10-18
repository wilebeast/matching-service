package matching

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"testing"
)

func fixture(t testing.TB) Config {
	t.Helper()
	b, err := os.ReadFile("../../examples/config.json")
	if err != nil {
		t.Fatal(err)
	}
	var c Config
	if err = json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c
}
func request() Request {
	return Request{Scene: "job_search", CandidateType: "job", Context: map[string]any{"user": map[string]any{"member": true, "skills": []any{"java", "go"}, "experience": float64(5)}, "request": map[string]any{"city": "shanghai"}}}
}
func lit(v any) *Value { b, _ := json.Marshal(v); return &Value{Literal: b} }
func TestPipeline(t *testing.T) {
	e, err := New(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.Match(request())
	if err != nil {
		t.Fatal(err)
	}
	if r.Total != 1 || r.Matches[0].Candidate.ID != "800000000001" || len(r.Matches[0].Strategies) != 2 || r.RejectedByEligibility != 1 {
		t.Fatalf("unexpected result: %+v", r)
	}
	req := request()
	delete(req.Context["user"].(map[string]any), "member")
	r, err = e.Match(req)
	if err != nil || r.Total != 0 {
		t.Fatalf("missing user field: %+v %v", r, err)
	}
}
func TestMissingAndNot(t *testing.T) {
	c := fixture(t)
	c.Candidates[0].Attributes = map[string]any{"skills": []any{"java"}}
	c.Strategies = c.Strategies[:1]
	c.Strategies[0].Query = Rule{Not: &Rule{Field: "candidate.city", Op: "eq", Value: lit("beijing")}}
	e, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.Match(request())
	if err != nil {
		t.Fatal(err)
	}
	if r.Total != 0 {
		t.Fatalf("missing city must remain unknown: %+v", r)
	}
	c.Strategies[0].When = Rule{Not: &Rule{Field: "user.member", Op: "eq", Value: lit(true)}}
	e, err = New(c)
	if err != nil {
		t.Fatal(err)
	}
	req := request()
	delete(req.Context["user"].(map[string]any), "member")
	r, err = e.Match(req)
	if err != nil || len(r.MatchedStrategies) != 0 {
		t.Fatalf("NOT missing must remain unknown: %+v %v", r, err)
	}
}
func TestValidation(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Config)
	}{
		{"unknown field", func(c *Config) { c.Strategies[0].Query = Rule{Field: "candidate.nope", Op: "eq", Value: lit("x")} }},
		{"wrong reference", func(c *Config) {
			c.Strategies[0].Query = Rule{Field: "candidate.city", Op: "eq", Value: &Value{Ref: "user.skills"}}
		}},
		{"empty group", func(c *Config) { c.Strategies[0].When = Rule{All: []Rule{}} }},
		{"duplicate id", func(c *Config) { c.Candidates = append(c.Candidates, c.Candidates[0]) }},
		{"bad bigint", func(c *Config) { c.Candidates[0].ID = "not-an-id" }},
		{"schema version", func(c *Config) { c.Candidates[0].SchemaVersion = 2 }},
		{"ambiguous node", func(c *Config) { c.Strategies[0].When.All = []Rule{c.Strategies[0].When} }},
		{"bad attribute", func(c *Config) { c.Candidates[0].Attributes["city"] = float64(42) }},
		{"empty operand", func(c *Config) {
			c.Strategies[0].Query = Rule{Field: "candidate.skills", Op: "contains_all", Value: lit([]string{})}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fixture(t)
			tc.change(&c)
			if _, err := New(c); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}
func TestContextValidation(t *testing.T) {
	e, err := New(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []any{"java", []any{}, []any{true}} {
		req := request()
		req.Context["user"].(map[string]any)["skills"] = v
		if _, err = e.Match(req); err == nil {
			t.Fatalf("accepted invalid skills %v", v)
		}
	}
}
func TestBitmapMatchesScan(t *testing.T) {
	c := fixture(t)
	c.Candidates = nil
	c.Strategies = c.Strategies[:1]
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 2000; i++ {
		attrs := map[string]any{"salary": float64(rng.Intn(100))}
		if rng.Intn(4) != 0 {
			attrs["city"] = []string{"shanghai", "beijing"}[rng.Intn(2)]
		}
		c.Candidates = append(c.Candidates, Candidate{ID: fmt.Sprint(800000000000 + int64(i)), Type: "job", SchemaVersion: 1, Version: 1, Status: "active", Attributes: attrs})
	}
	c.Strategies[0].Query = Rule{All: []Rule{{Not: &Rule{Field: "candidate.city", Op: "eq", Value: lit("beijing")}}, {Field: "candidate.salary", Op: "gte", Value: lit(50)}}}
	e, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.Match(request())
	if err != nil {
		t.Fatal(err)
	}
	want := 0
	for _, c := range c.Candidates {
		if c.Attributes["city"] == "shanghai" && c.Attributes["salary"].(float64) >= 50 {
			want++
		}
	}
	if r.Total != want {
		t.Fatalf("bitmap=%d scan=%d", r.Total, want)
	}
}
func TestImmutableAndConcurrent(t *testing.T) {
	c := fixture(t)
	e, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	c.Candidates[0].Status = "inactive"
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				r, err := e.Match(request())
				if err != nil || r.Total != 1 {
					t.Errorf("unexpected concurrent result: %+v %v", r, err)
				}
			}
		}()
	}
	wg.Wait()
}

func TestOperators(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rule  Rule
		count int
	}{
		{"eq", Rule{Field: "candidate.city", Op: "eq", Value: lit("shanghai")}, 2},
		{"ne", Rule{Field: "candidate.city", Op: "ne", Value: lit("shanghai")}, 1},
		{"in", Rule{Field: "candidate.city", Op: "in", Value: lit([]string{"beijing", "shanghai"})}, 3},
		{"any skill", Rule{Field: "candidate.skills", Op: "contains_any", Value: lit([]string{"mysql", "go"})}, 2},
		{"all skills", Rule{Field: "candidate.skills", Op: "contains_all", Value: lit([]string{"java", "mysql"})}, 1},
		{"exists", Rule{Field: "candidate.city", Op: "exists"}, 3},
		{"range", Rule{Field: "candidate.salary", Op: "gt", Value: lit(30000)}, 0},
		{"or", Rule{Any: []Rule{{Field: "candidate.city", Op: "eq", Value: lit("beijing")}, {Field: "candidate.skills", Op: "contains_any", Value: lit([]string{"mysql"})}}}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fixture(t)
			c.Strategies = c.Strategies[:1]
			c.Strategies[0].Query = tc.rule
			for i := range c.Candidates {
				c.Candidates[i].Eligibility = nil
			}
			e, err := New(c)
			if err != nil {
				t.Fatal(err)
			}
			r, err := e.Match(request())
			if err != nil || r.Total != tc.count {
				t.Fatalf("count=%d want=%d err=%v", r.Total, tc.count, err)
			}
		})
	}
}
