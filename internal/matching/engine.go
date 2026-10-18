package matching

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"github.com/RoaringBitmap/roaring/v2"
)

type Field struct {
	Type    string `json:"type"`
	Indexed bool   `json:"indexed"`
}
type Schema struct {
	Version int              `json:"version"`
	Fields  map[string]Field `json:"fields"`
}
type Value struct {
	Literal json.RawMessage `json:"literal,omitempty"`
	Ref     string          `json:"ref,omitempty"`
}
type Rule struct {
	All   []Rule `json:"all,omitempty"`
	Any   []Rule `json:"any,omitempty"`
	Not   *Rule  `json:"not,omitempty"`
	Field string `json:"field,omitempty"`
	Op    string `json:"op,omitempty"`
	Value *Value `json:"value,omitempty"`
}
type Candidate struct {
	ID            string         `json:"id"`
	Type          string         `json:"type"`
	SchemaVersion int            `json:"schema_version"`
	Version       uint64         `json:"version"`
	Status        string         `json:"status"`
	Attributes    map[string]any `json:"attributes"`
	Eligibility   *Rule          `json:"eligibility,omitempty"`
}
type Scope struct {
	Scene         string `json:"scene"`
	CandidateType string `json:"candidate_type"`
}
type Strategy struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
	Enabled bool   `json:"enabled"`
	Scope   Scope  `json:"scope"`
	When    Rule   `json:"when"`
	Query   Rule   `json:"query"`
}
type Config struct {
	Version       uint64            `json:"version"`
	ContextSchema Schema            `json:"context_schema"`
	Schemas       map[string]Schema `json:"schemas"`
	Strategies    []Strategy        `json:"strategies"`
	Candidates    []Candidate       `json:"candidates"`
}
type Request struct {
	Scene         string         `json:"scene"`
	CandidateType string         `json:"candidate_type"`
	Context       map[string]any `json:"context"`
	Limit         int            `json:"limit"`
}
type Match struct {
	Candidate  Candidate `json:"candidate"`
	LocalID    uint32    `json:"local_id"`
	Strategies []string  `json:"strategies"`
}
type Result struct {
	Version               uint64   `json:"version"`
	ShardID               int      `json:"shard_id"`
	MatchedStrategies     []string `json:"matched_strategies"`
	Matches               []Match  `json:"matches"`
	Total                 int      `json:"total"`
	RejectedByEligibility int      `json:"rejected_by_eligibility"`
}
type pair struct{ yes, no *roaring.Bitmap }
type program struct {
	strategy Strategy
	when     cel.Program
}
type Engine struct {
	config      Config
	programs    []program
	candidates  map[uint32]Candidate
	eligibility map[uint32]cel.Program
	index       map[string]map[string]*roaring.Bitmap
	present     map[string]*roaring.Bitmap
	universe    map[string]*roaring.Bitmap
}

func indexField(t, f string) string { return t + "/" + f }
func token(v any) string {
	// IEEE-754 positive and negative zero compare equal and share one index key.
	if n, ok := v.(float64); ok && n == 0 {
		v = float64(0)
	}
	b, _ := json.Marshal(v)
	return string(b)
}
func validPath(s string) bool {
	if s == "" {
		return false
	}
	for _, part := range strings.Split(s, ".") {
		if part == "" {
			return false
		}
		for i, c := range part {
			if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9') {
				return false
			}
		}
	}
	return true
}
func validType(t string) bool {
	return t == "keyword" || t == "keyword[]" || t == "number" || t == "boolean"
}
func validValue(t string, v any) bool {
	switch t {
	case "keyword":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "number":
		x, ok := v.(float64)
		return ok && !math.IsNaN(x) && !math.IsInf(x, 0) && math.Abs(x) <= 9007199254740991
	case "keyword[]":
		a, ok := v.([]any)
		if !ok || len(a) > 256 {
			return false
		}
		for _, x := range a {
			if _, ok := x.(string); !ok {
				return false
			}
		}
		return true
	}
	return false
}
func lookup(ctx map[string]any, path string) (any, bool) {
	var v any = ctx
	for _, p := range strings.Split(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		v, ok = m[p]
		if !ok || v == nil {
			return nil, false
		}
	}
	return v, true
}
func validateSchema(s Schema) error {
	if s.Version < 1 || len(s.Fields) == 0 {
		return fmt.Errorf("schema version and fields required")
	}
	for n, f := range s.Fields {
		if !validPath(n) || !validType(f.Type) {
			return fmt.Errorf("invalid field %s", n)
		}
	}
	return nil
}
func validateRule(r Rule, fields map[string]Field, context map[string]Field, query bool, depth int, nodes *int) error {
	*nodes++
	if depth > 16 || *nodes > 256 {
		return fmt.Errorf("rule exceeds depth/node limit")
	}
	forms := 0
	if r.All != nil {
		forms++
	}
	if r.Any != nil {
		forms++
	}
	if r.Not != nil {
		forms++
	}
	if r.Field != "" || r.Op != "" || r.Value != nil {
		forms++
	}
	if forms != 1 {
		return fmt.Errorf("rule must have exactly one node kind")
	}
	if r.Not != nil {
		return validateRule(*r.Not, fields, context, query, depth+1, nodes)
	}
	if r.All != nil || r.Any != nil {
		children := r.All
		if r.Any != nil {
			children = r.Any
		}
		if len(children) == 0 {
			return fmt.Errorf("empty condition group")
		}
		for _, c := range children {
			if err := validateRule(c, fields, context, query, depth+1, nodes); err != nil {
				return err
			}
		}
		return nil
	}
	name := r.Field
	if query {
		if !strings.HasPrefix(name, "candidate.") {
			return fmt.Errorf("query field must start candidate.")
		}
		name = strings.TrimPrefix(name, "candidate.")
	}
	f, ok := fields[name]
	if !ok {
		return fmt.Errorf("unknown field %s", r.Field)
	}
	if query && !f.Indexed {
		return fmt.Errorf("field %s is not indexed", name)
	}
	if r.Op == "exists" {
		if r.Value != nil {
			return fmt.Errorf("exists takes no value")
		}
		return nil
	}
	if r.Value == nil {
		return fmt.Errorf("value required")
	}
	if (r.Value.Ref != "") == (len(r.Value.Literal) > 0) {
		return fmt.Errorf("exactly one of literal or ref required")
	}
	expected := f.Type
	switch r.Op {
	case "eq", "ne":
		if f.Type == "keyword[]" {
			return fmt.Errorf("use contains_any/all for arrays")
		}
	case "in":
		if f.Type != "keyword" {
			return fmt.Errorf("in requires keyword field")
		}
		expected = "keyword[]"
	case "contains_any", "contains_all":
		if f.Type != "keyword[]" {
			return fmt.Errorf("contains requires array field")
		}
	case "gt", "gte", "lt", "lte":
		if f.Type != "number" {
			return fmt.Errorf("range requires number field")
		}
	default:
		return fmt.Errorf("unsupported operator %s", r.Op)
	}
	if r.Value.Ref != "" {
		ref, ok := context[r.Value.Ref]
		if !ok || ref.Type != expected {
			return fmt.Errorf("invalid reference %s (want %s)", r.Value.Ref, expected)
		}
	} else {
		var v any
		if err := json.Unmarshal(r.Value.Literal, &v); err != nil || !validValue(expected, v) {
			return fmt.Errorf("invalid literal for %s", r.Field)
		}
		if a, ok := v.([]any); ok && len(a) == 0 {
			return fmt.Errorf("empty operand collection")
		}
	}
	return nil
}

// Each node compiles to true/false predicates; missing fields match neither.
func celPath(path string) (string, string) {
	expr := "ctx"
	guards := []string{}
	for _, p := range strings.Split(path, ".") {
		q := strconv.Quote(p)
		guards = append(guards, "("+q+" in "+expr+")")
		expr += "[" + q + "]"
	}
	guards = append(guards, "("+expr+" != null)")
	return expr, strings.Join(guards, " && ")
}
func expressions(r Rule, literals *[]any) (string, string) {
	if r.Not != nil {
		t, f := expressions(*r.Not, literals)
		return f, t
	}
	if r.All != nil || r.Any != nil {
		children := r.All
		top, fop := " && ", " || "
		if r.Any != nil {
			children = r.Any
			top, fop = " || ", " && "
		}
		ts, fs := []string{}, []string{}
		for _, c := range children {
			t, f := expressions(c, literals)
			ts = append(ts, "("+t+")")
			fs = append(fs, "("+f+")")
		}
		return strings.Join(ts, top), strings.Join(fs, fop)
	}
	a, g := celPath(r.Field)
	if r.Op == "exists" {
		return "(" + g + ")", "!(" + g + ")"
	}
	b := ""
	if r.Value.Ref != "" {
		var rg string
		b, rg = celPath(r.Value.Ref)
		g += " && (" + rg + ")"
	} else {
		// JSON escaping is not CEL syntax. Bind decoded values as data.
		var value any
		_ = json.Unmarshal(r.Value.Literal, &value) // already validated before compilation
		b = fmt.Sprintf("literals[%d]", len(*literals))
		*literals = append(*literals, value)
	}
	e := ""
	switch r.Op {
	case "eq":
		e = a + " == " + b
	case "ne":
		e = a + " != " + b
	case "gt":
		e = a + " > " + b
	case "gte":
		e = a + " >= " + b
	case "lt":
		e = a + " < " + b
	case "lte":
		e = a + " <= " + b
	case "in":
		e = a + " in " + b
	case "contains_any":
		e = a + ".exists(x, x in " + b + ")"
	case "contains_all":
		e = "(" + b + ").all(x, x in " + a + ")"
	}
	return "(" + g + ") && (" + e + ")", "(" + g + ") && !(" + e + ")"
}
func compile(env *cel.Env, r Rule) (cel.Program, error) {
	literals := []any{}
	t, _ := expressions(r, &literals)
	ast, issues := env.Compile(t)
	if issues.Err() != nil {
		return nil, issues.Err()
	}
	return env.Program(ast, cel.Globals(map[string]any{"literals": literals}), cel.CostLimit(10000))
}
func New(config Config) (*Engine, error) {
	// Take an owned JSON-normalized copy; callers cannot mutate an active snapshot.
	b, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(b, &config); err != nil {
		return nil, err
	}
	if config.Version == 0 {
		return nil, fmt.Errorf("config version required")
	}
	if err = validateSchema(config.ContextSchema); err != nil {
		return nil, err
	}
	if len(config.Schemas) == 0 {
		return nil, fmt.Errorf("candidate schemas required")
	}
	for t, s := range config.Schemas {
		if !validPath(t) {
			return nil, fmt.Errorf("invalid candidate type")
		}
		if err = validateSchema(s); err != nil {
			return nil, err
		}
		for f := range s.Fields {
			if strings.Contains(f, ".") {
				return nil, fmt.Errorf("candidate attributes must be flat")
			}
		}
	}
	env, err := cel.NewEnv(cel.Variable("ctx", cel.MapType(cel.StringType, cel.DynType)), cel.Variable("literals", cel.ListType(cel.DynType)))
	if err != nil {
		return nil, err
	}
	e := &Engine{config: config, candidates: map[uint32]Candidate{}, eligibility: map[uint32]cel.Program{}, index: map[string]map[string]*roaring.Bitmap{}, present: map[string]*roaring.Bitmap{}, universe: map[string]*roaring.Bitmap{}}
	seen := map[string]bool{}
	for _, s := range config.Strategies {
		if s.ID == "" || seen[s.ID] || s.Version < 1 || s.Scope.Scene == "" {
			return nil, fmt.Errorf("invalid/duplicate strategy")
		}
		seen[s.ID] = true
		schema, ok := config.Schemas[s.Scope.CandidateType]
		if !ok {
			return nil, fmt.Errorf("unknown candidate type")
		}
		n := 0
		if err = validateRule(s.When, config.ContextSchema.Fields, config.ContextSchema.Fields, false, 0, &n); err != nil {
			return nil, fmt.Errorf("strategy %s when: %w", s.ID, err)
		}
		n = 0
		if err = validateRule(s.Query, schema.Fields, config.ContextSchema.Fields, true, 0, &n); err != nil {
			return nil, fmt.Errorf("strategy %s query: %w", s.ID, err)
		}
		p, err := compile(env, s.When)
		if err != nil {
			return nil, err
		}
		e.programs = append(e.programs, program{s, p})
	}
	seen = map[string]bool{}
	if uint64(len(config.Candidates)) > uint64(math.MaxUint32) {
		return nil, fmt.Errorf("shard exhausted")
	}
	for i, c := range config.Candidates {
		businessID, err := strconv.ParseInt(c.ID, 10, 64)
		if err != nil || businessID <= 0 || strconv.FormatInt(businessID, 10) != c.ID {
			return nil, fmt.Errorf("candidate id must be positive signed bigint string")
		}
		key := c.Type + "/" + c.ID
		if seen[key] {
			return nil, fmt.Errorf("duplicate candidate %s", key)
		}
		seen[key] = true
		schema, ok := config.Schemas[c.Type]
		if !ok || c.SchemaVersion != schema.Version || c.Version == 0 || (c.Status != "active" && c.Status != "inactive") {
			return nil, fmt.Errorf("invalid candidate metadata %s", key)
		}
		for name, v := range c.Attributes {
			f, ok := schema.Fields[name]
			if !ok || !validValue(f.Type, v) {
				return nil, fmt.Errorf("invalid attribute %s", name)
			}
		}
		id := uint32(i)
		e.candidates[id] = c
		if c.Eligibility != nil {
			n := 0
			if err = validateRule(*c.Eligibility, config.ContextSchema.Fields, config.ContextSchema.Fields, false, 0, &n); err != nil {
				return nil, err
			}
			p, err := compile(env, *c.Eligibility)
			if err != nil {
				return nil, err
			}
			e.eligibility[id] = p
		}
		if c.Status != "active" {
			continue
		}
		if e.universe[c.Type] == nil {
			e.universe[c.Type] = roaring.New()
		}
		e.universe[c.Type].Add(id)
		for name, v := range c.Attributes {
			if !schema.Fields[name].Indexed {
				continue
			}
			f := indexField(c.Type, name)
			if e.index[f] == nil {
				e.index[f] = map[string]*roaring.Bitmap{}
				e.present[f] = roaring.New()
			}
			e.present[f].Add(id)
			values := []any{v}
			if a, ok := v.([]any); ok {
				values = a
			}
			for _, x := range values {
				k := token(x)
				if e.index[f][k] == nil {
					e.index[f][k] = roaring.New()
				}
				e.index[f][k].Add(id)
			}
		}
	}
	return e, nil
}
func empty() *roaring.Bitmap { return roaring.New() }
func (e *Engine) leaf(t, f string, v any) *roaring.Bitmap {
	if b := e.index[indexField(t, f)][token(v)]; b != nil {
		return b.Clone()
	}
	return empty()
}
func (e *Engine) query(t string, r Rule, ctx map[string]any, u *roaring.Bitmap) pair {
	if r.Not != nil {
		p := e.query(t, *r.Not, ctx, u)
		return pair{p.no, p.yes}
	}
	if r.All != nil || r.Any != nil {
		children := r.All
		all := r.All != nil
		if !all {
			children = r.Any
		}
		p := pair{u.Clone(), empty()}
		if !all {
			p = pair{empty(), u.Clone()}
		}
		for _, c := range children {
			q := e.query(t, c, ctx, u)
			if all {
				p.yes.And(q.yes)
				p.no.Or(q.no)
			} else {
				p.yes.Or(q.yes)
				p.no.And(q.no)
			}
		}
		return p
	}
	f := strings.TrimPrefix(r.Field, "candidate.")
	present := empty()
	if b := e.present[indexField(t, f)]; b != nil {
		present = b.Clone()
	}
	present.And(u)
	if r.Op == "exists" {
		return pair{present, roaring.AndNot(u, present)}
	}
	var v any
	if r.Value.Ref != "" {
		var ok bool
		v, ok = lookup(ctx, r.Value.Ref)
		if !ok {
			return pair{empty(), empty()}
		}
	} else {
		_ = json.Unmarshal(r.Value.Literal, &v)
	}
	yes := empty()
	switch r.Op {
	case "eq", "ne":
		yes = e.leaf(t, f, v)
		if r.Op == "ne" {
			yes = roaring.AndNot(present, yes)
		}
	case "in", "contains_any", "contains_all":
		a := v.([]any)
		if r.Op == "contains_all" {
			yes = present.Clone()
		}
		for _, x := range a {
			b := e.leaf(t, f, x)
			if r.Op == "contains_all" {
				yes.And(b)
			} else {
				yes.Or(b)
			}
		}
	case "gt", "gte", "lt", "lte":
		n := v.(float64)
		for k, b := range e.index[indexField(t, f)] {
			x, _ := strconv.ParseFloat(k, 64)
			ok := r.Op == "gt" && x > n || r.Op == "gte" && x >= n || r.Op == "lt" && x < n || r.Op == "lte" && x <= n
			if ok {
				yes.Or(b)
			}
		}
	}
	yes.And(present)
	return pair{yes, roaring.AndNot(present, yes)}
}
func evaluate(p cel.Program, ctx map[string]any) (bool, error) {
	v, _, err := p.Eval(map[string]any{"ctx": ctx})
	if err != nil {
		return false, err
	}
	return v == types.True, nil
}
func validateContext(ctx map[string]any, fields map[string]Field, prefix string) error {
	for k, v := range ctx {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		if f, ok := fields[path]; ok {
			if !validValue(f.Type, v) {
				return fmt.Errorf("invalid context field %s", path)
			}
			if a, ok := v.([]any); ok && len(a) == 0 {
				return fmt.Errorf("empty context collection %s", path)
			}
			continue
		}
		m, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("unknown context field %s", path)
		}
		known := false
		for f := range fields {
			if strings.HasPrefix(f, path+".") {
				known = true
				break
			}
		}
		if !known {
			return fmt.Errorf("unknown context object %s", path)
		}
		if err := validateContext(m, fields, path); err != nil {
			return err
		}
	}
	return nil
}
func (e *Engine) Match(req Request) (Result, error) {
	result := Result{Version: e.config.Version, MatchedStrategies: []string{}, Matches: []Match{}}
	if req.Scene == "" || req.CandidateType == "" {
		return result, fmt.Errorf("scene and candidate_type required")
	}
	if _, ok := e.config.Schemas[req.CandidateType]; !ok {
		return result, fmt.Errorf("unknown candidate type")
	}
	if req.Limit < 0 || req.Limit > 1000 {
		return result, fmt.Errorf("limit must be 0..1000")
	}
	if req.Limit == 0 {
		req.Limit = 20
	}
	if err := validateContext(req.Context, e.config.ContextSchema.Fields, ""); err != nil {
		return result, err
	}
	u := e.universe[req.CandidateType]
	if u == nil {
		u = empty()
	}
	hits := map[uint32][]string{}
	for _, p := range e.programs {
		s := p.strategy
		if !s.Enabled || s.Scope.Scene != req.Scene || s.Scope.CandidateType != req.CandidateType {
			continue
		}
		ok, err := evaluate(p.when, req.Context)
		if err != nil {
			return result, fmt.Errorf("strategy %s evaluation: %w", s.ID, err)
		}
		if !ok {
			continue
		}
		result.MatchedStrategies = append(result.MatchedStrategies, s.ID)
		ids := e.query(req.CandidateType, s.Query, req.Context, u).yes
		it := ids.Iterator()
		for it.HasNext() {
			id := it.Next()
			hits[id] = append(hits[id], s.ID)
		}
	}
	for id, strategies := range hits {
		if p := e.eligibility[id]; p != nil {
			ok, err := evaluate(p, req.Context)
			if err != nil {
				return result, fmt.Errorf("candidate eligibility: %w", err)
			}
			if !ok {
				result.RejectedByEligibility++
				continue
			}
		}
		sort.Strings(strategies)
		result.Matches = append(result.Matches, Match{e.candidates[id], id, strategies})
	}
	sort.Slice(result.Matches, func(i, j int) bool {
		a, b := result.Matches[i], result.Matches[j]
		if len(a.Strategies) != len(b.Strategies) {
			return len(a.Strategies) > len(b.Strategies)
		}
		ai, _ := strconv.ParseInt(a.Candidate.ID, 10, 64)
		bi, _ := strconv.ParseInt(b.Candidate.ID, 10, 64)
		return ai < bi
	})
	result.Total = len(result.Matches)
	if len(result.Matches) > req.Limit {
		result.Matches = result.Matches[:req.Limit]
	}
	return result, nil
}
