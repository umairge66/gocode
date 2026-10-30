package insights

import (
	"context"
	"strings"
	"sync"
)

// facetMap is an insertion-ordered session-id → facets map (a JS Map).
type facetMap struct {
	keys []string
	vals map[string]*Facets
}

func newFacetMap() *facetMap { return &facetMap{vals: map[string]*Facets{}} }

func (m *facetMap) set(k string, f *Facets) {
	if _, ok := m.vals[k]; !ok {
		m.keys = append(m.keys, k)
	}
	m.vals[k] = f
}

func (m *facetMap) get(k string) *Facets {
	if m == nil {
		return nil
	}
	return m.vals[k]
}

func (m *facetMap) len() int {
	if m == nil {
		return 0
	}
	return len(m.keys)
}

func (m *facetMap) values() []*Facets {
	out := make([]*Facets, 0, len(m.keys))
	for _, k := range m.keys {
		out = append(out, m.vals[k])
	}
	return out
}

// insightSection is one synthesis query.
type insightSection struct {
	name      string
	prompt    string
	maxTokens int
}

// sectionResult is a named parsed reply.
type sectionResult struct {
	name   string
	result *jsObject
}

// runSection sends one section query (prompt + "\n\nDATA:\n" + data) and
// parses the first {...} of the reply. Failures yield a nil result.
func (g *generator) runSection(ctx context.Context, sec insightSection, data string) sectionResult {
	out, err := g.opts.Model(ctx, "", sec.prompt+sectionDataSeparator+data, sec.maxTokens)
	if err != nil || out == "" {
		return sectionResult{name: sec.name}
	}
	m := firstJSONObject.FindString(out)
	if m == "" {
		return sectionResult{name: sec.name}
	}
	v, err := parseJSValue([]byte(m))
	if err != nil {
		return sectionResult{name: sec.name}
	}
	o, _ := v.(*jsObject)
	if o == nil {
		// JSON.parse succeeds for non-objects too; only objects are usable.
		return sectionResult{name: sec.name}
	}
	return sectionResult{name: sec.name, result: o}
}

// topPairs renders [[name,count],...] for the top-N entries.
func topPairs(c *counts, n int) []any {
	out := []any{}
	for i, e := range c.sortedDesc() {
		if i >= n {
			break
		}
		out = append(out, []any{e.key, e.val})
	}
	return out
}

// bulletList renders "- a: b" lines from an array of objects.
func bulletList(arr []any, k1, k2 string) string {
	var lines []string
	for _, it := range arr {
		o, _ := it.(*jsObject)
		lines = append(lines, "- "+o.field(k1)+": "+o.field(k2))
	}
	return strings.Join(lines, "\n")
}

// synthesize runs the parallel section queries over the aggregate and facet
// summaries, then the "At a Glance" query over their results. The returned
// object holds each successful section by name, in section order, with
// at_a_glance last.
func (g *generator) synthesize(ctx context.Context, d *AggregatedData, facets *facetMap) *jsObject {
	vals := facets.values()
	var summaries, frictions, instructions []string
	for i, c := range vals {
		if i >= 50 {
			break
		}
		summaries = append(summaries, "- "+c.obj.field("brief_summary")+" ("+c.obj.field("outcome")+", "+c.obj.field("gocode_helpfulness")+")")
	}
	for _, c := range vals {
		if len(frictions) >= 20 {
			break
		}
		if c.obj.truthy("friction_detail") {
			frictions = append(frictions, "- "+c.obj.field("friction_detail"))
		}
	}
	for _, c := range vals {
		v, ok := c.obj.get("user_instructions_to_gocode")
		if !ok || !jsTruthy(v) {
			continue
		}
		arr, isArr := v.([]any)
		if !isArr {
			arr = []any{v} // flatMap keeps a non-array value as one item
		}
		for _, x := range arr {
			if len(instructions) >= 15 {
				break
			}
			instructions = append(instructions, "- "+jsString(x, true))
		}
	}
	data := newJSObject()
	data.set("sessions", float64(d.TotalSessions))
	data.set("analyzed", float64(d.SessionsWithFacets))
	dr := newJSObject()
	dr.set("start", d.DateRange.Start)
	dr.set("end", d.DateRange.End)
	data.set("date_range", dr)
	data.set("messages", float64(d.TotalMessages))
	data.set("hours", jsRound(d.TotalDurationHours))
	data.set("commits", float64(d.GitCommits))
	data.set("top_tools", topPairs(d.ToolCounts, 8))
	data.set("top_goals", topPairs(d.GoalCategories, 8))
	data.set("outcomes", d.Outcomes.toObject())
	data.set("satisfaction", d.Satisfaction.toObject())
	data.set("friction", d.Friction.toObject())
	data.set("success", d.Success.toObject())
	data.set("languages", d.Languages.toObject())
	instr := strings.Join(instructions, "\n")
	if instr == "" {
		instr = "None captured"
	}
	y := stringifyJS(data, "  ") +
		"\n\nSESSION SUMMARIES:\n" + strings.Join(summaries, "\n") +
		"\n\nFRICTION DETAILS:\n" + strings.Join(frictions, "\n") +
		"\n\nUSER INSTRUCTIONS TO gocode:\n" + instr

	results := make([]sectionResult, len(insightSections))
	var wg sync.WaitGroup
	for i, sec := range insightSections {
		wg.Add(1)
		go func(i int, sec insightSection) {
			defer wg.Done()
			results[i] = g.runSection(ctx, sec, y)
		}(i, sec)
	}
	wg.Wait()
	r := newJSObject()
	for _, res := range results {
		if res.result != nil {
			r.set(res.name, res.result)
		}
	}
	f := bulletList(r.obj("project_areas").arr("areas"), "name", "description")
	h := bulletList(r.obj("what_works").arr("impressive_workflows"), "title", "description")
	a := bulletList(r.obj("friction_analysis").arr("categories"), "category", "description")
	rr := bulletList(r.obj("suggestions").arr("features_to_try"), "feature", "one_liner")
	i := bulletList(r.obj("suggestions").arr("usage_patterns"), "title", "suggestion")
	p := bulletList(r.obj("on_the_horizon").arr("opportunities"), "title", "whats_possible")
	glance := insightSection{
		name:      "at_a_glance",
		prompt:    fill(atAGlancePromptTpl, y, f, h, a, rr, i, p),
		maxTokens: 8192,
	}
	if b := g.runSection(ctx, glance, ""); b.result != nil {
		r.set("at_a_glance", b.result)
	}
	return r
}
