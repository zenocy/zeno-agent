package eval

import (
	"fmt"
	"html/template"
	"io"
	"strings"
	"time"
)

// ReportData is the top-level shape passed to the HTML template.
type ReportData struct {
	GeneratedAt     time.Time
	Model           string
	Endpoint        string
	Results         []*RunResult      // morning fixtures
	ReactiveResults []*ReactiveResult // reactive fixtures (rendered in a separate section)
}

// HasMustCards reports whether any result in the report exercises must-card
// expectations. The template uses this to hide the column when no fixture
// declares any.
func (d ReportData) HasMustCards() bool {
	for _, r := range d.Results {
		if len(r.Scoreboard.MustCards) > 0 {
			return true
		}
	}
	return false
}

// HasMemory reports whether any result has seeded memory facts. Used to
// gate the memory_grounding column so memory-free runs stay clean.
func (d ReportData) HasMemory() bool {
	for _, r := range d.Results {
		if r.Scoreboard.MemoryGrounding.FactsInjected > 0 {
			return true
		}
	}
	for _, r := range d.ReactiveResults {
		if r.Scoreboard.MemoryGrounding.FactsInjected > 0 {
			return true
		}
	}
	return false
}

// HasState reports whether any result is scored against an
// expected_state. Used to gate the state_match column so pre-V2.3 corpora
// stay clean.
func (d ReportData) HasState() bool {
	for _, r := range d.Results {
		if r.Scoreboard.StateMatch.Expected != "" {
			return true
		}
	}
	return false
}

// HasReactive reports whether any reactive fixture was scored. Used to gate
// the reactive section in the HTML report so morning-only runs stay clean.
func (d ReportData) HasReactive() bool {
	return len(d.ReactiveResults) > 0
}

const reportTemplate = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>Zeno Eval — {{.GeneratedAt.Format "2006-01-02 15:04"}}</title>
  <style>
    body { font-family: ui-monospace, "SF Mono", Menlo, monospace; max-width: 1100px; margin: 2rem auto; padding: 0 1rem; color: #111; background: #fafaf7; }
    h1 { font-family: Georgia, serif; font-weight: 400; font-size: 1.6rem; }
    .meta { color: #666; font-size: 0.85rem; margin-bottom: 1.5rem; }
    table { border-collapse: collapse; width: 100%; margin-bottom: 2rem; }
    th, td { text-align: left; padding: 0.5rem 0.75rem; border-bottom: 1px solid #e3e1d8; vertical-align: top; }
    th { background: #f0eee6; font-weight: 600; font-size: 0.8rem; text-transform: uppercase; letter-spacing: 0.05em; }
    .score-3 { color: #2f7a4d; }
    .score-2 { color: #b87b1e; }
    .score-1 { color: #b8331e; }
    .score-0 { color: #b8331e; font-weight: 600; }
    .ok  { color: #2f7a4d; }
    .err { color: #b8331e; }
    pre { background: #fff; border: 1px solid #e3e1d8; padding: 0.75rem; white-space: pre-wrap; font-size: 0.8rem; }
    .fixture { margin-bottom: 3rem; padding: 1rem; background: #fff; border: 1px solid #e3e1d8; border-radius: 6px; }
    .fixture h2 { margin-top: 0; font-family: Georgia, serif; font-weight: 400; }
    .badge { display: inline-block; padding: 2px 8px; border-radius: 12px; font-size: 0.7rem; background: #e3e1d8; }
    .badge.degraded { background: #b87b1e; color: #fff; }
    .hits { color: #b8331e; font-size: 0.75rem; }
  </style>
</head>
<body>
  <h1>Zeno Eval Report</h1>
  <p class="meta">
    Generated {{.GeneratedAt.Format "2006-01-02 15:04:05 MST"}} ·
    Model <strong>{{.Model}}</strong> ·
    Endpoint <code>{{.Endpoint}}</code>
  </p>

  <table>
    <thead>
      <tr>
        <th>Fixture</th>
        <th>Calm</th>
        <th>Decisive</th>
        <th>Concrete</th>
        <th>Total /9</th>
        <th>Briefing schema</th>
        <th>Cards schema</th>
        <th>Italic bal.</th>
        {{if .HasMustCards}}<th>Must cards</th>{{end}}
        {{if .HasMemory}}<th>Memory grounding</th>{{end}}
        {{if .HasState}}<th>State</th>{{end}}
        {{if .HasState}}<th>Tension band</th>{{end}}
        <th>Latency</th>
        <th>Status</th>
      </tr>
    </thead>
    <tbody>
      {{range .Results}}
      <tr>
        <td><a href="#{{.FixtureName}}">{{.FixtureName}}</a></td>
        <td class="score-{{.Scoreboard.Calmness.Value}}">{{.Scoreboard.Calmness.Value}}</td>
        <td class="score-{{.Scoreboard.Decisiveness.Value}}">{{.Scoreboard.Decisiveness.Value}}</td>
        <td class="score-{{.Scoreboard.Concreteness.Value}}">{{.Scoreboard.Concreteness.Value}}</td>
        <td><strong>{{.Scoreboard.Total}}</strong></td>
        <td>{{if .Scoreboard.BriefingSchema}}<span class="ok">✓</span>{{else}}<span class="err">✗</span>{{end}}</td>
        <td>{{if .Scoreboard.CardsSchema}}<span class="ok">✓</span>{{else}}<span class="err">✗</span>{{end}}</td>
        <td>{{if .Scoreboard.ItalicBalanced}}<span class="ok">✓</span>{{else}}<span class="err">✗</span>{{end}}</td>
        {{if $.HasMustCards}}<td>{{mustCardsSummary .Scoreboard}}</td>{{end}}
        {{if $.HasMemory}}<td>{{memoryGroundingSummary .Scoreboard.MemoryGrounding}}</td>{{end}}
        {{if $.HasState}}<td>{{stateMatchSummary .Scoreboard.StateMatch}}</td>{{end}}
        {{if $.HasState}}<td>{{tensionBandSummary .Scoreboard.TensionInBand}}</td>{{end}}
        <td>{{durationMs .TotalLatency}}ms</td>
        <td>{{if .BriefingDeg}}<span class="badge degraded">degraded</span>{{else if or .CardsErr .BriefingErr}}<span class="err">error</span>{{else}}<span class="ok">ok</span>{{end}}</td>
      </tr>
      {{end}}
    </tbody>
  </table>

  {{range .Results}}
  <div class="fixture" id="{{.FixtureName}}">
    <h2>{{.FixtureName}}</h2>

    <p>
      <strong>Eyebrow:</strong> {{.Briefing.Eyebrow}}<br>
      <strong>Title:</strong> {{.Briefing.Title}}<br>
      <strong>Tension:</strong> {{.Briefing.Tension}}
    </p>
    <p><strong>Summary:</strong></p>
    <pre>{{.Briefing.Summary}}</pre>

    <p><strong>Cards ({{len .Cards.Cards}}):</strong></p>
    {{range .Cards.Cards}}
    <pre><strong>{{.Title}}</strong> [{{.Source}} / {{.Rel}}]
{{.Sub}}</pre>
    {{end}}

    {{if or .Scoreboard.Calmness.Hits .Scoreboard.Decisiveness.Hits .Scoreboard.Concreteness.Hits}}
    <p><strong>Hits:</strong></p>
    <ul>
      {{range .Scoreboard.Calmness.Hits}}<li class="hits">calmness — {{.}}</li>{{end}}
      {{range .Scoreboard.Decisiveness.Hits}}<li class="hits">decisiveness — {{.}}</li>{{end}}
      {{range .Scoreboard.Concreteness.Hits}}<li class="hits">concreteness — {{.}}</li>{{end}}
    </ul>
    {{end}}

    {{if gt .Scoreboard.MemoryGrounding.FactsInjected 0}}
    <p><strong>Memory diff:</strong></p>
    <ul>
      <li>Facts injected: {{.Scoreboard.MemoryGrounding.FactsInjected}}</li>
      <li>Subjects referenced ({{len .Scoreboard.MemoryGrounding.Subjects}}): {{joinList .Scoreboard.MemoryGrounding.Subjects}}</li>
      <li>Verbatim leaks: {{if .Scoreboard.MemoryGrounding.VerbatimHits}}<span class="err">{{joinList .Scoreboard.MemoryGrounding.VerbatimHits}}</span>{{else}}<span class="ok">none</span>{{end}}</li>
      <li>Opener tells: {{if .Scoreboard.MemoryGrounding.OpenerTells.Hits}}<span class="err">{{joinList .Scoreboard.MemoryGrounding.OpenerTells.Hits}}</span>{{else}}<span class="ok">none</span>{{end}}</li>
    </ul>
    {{end}}

    {{if .CardsErr}}<p class="err">cards error: {{.CardsErr}}</p>{{end}}
    {{if .BriefingErr}}<p class="err">briefing error: {{.BriefingErr}}</p>{{end}}
  </div>
  {{end}}

  {{if .HasReactive}}
  <h1 style="margin-top: 3rem;">Reactive fixtures</h1>
  <table>
    <thead>
      <tr>
        <th>Fixture</th>
        <th>Calm</th>
        <th>Decisive</th>
        <th>Concrete</th>
        <th>Total /9</th>
        <th>Card schema</th>
        <th>Italic bal.</th>
        <th>Expect</th>
        <th>Latency</th>
        <th>Status</th>
      </tr>
    </thead>
    <tbody>
      {{range .ReactiveResults}}
      <tr>
        <td><a href="#r-{{.FixtureName}}">{{.FixtureName}}</a></td>
        <td class="score-{{.Scoreboard.Calmness.Value}}">{{.Scoreboard.Calmness.Value}}</td>
        <td class="score-{{.Scoreboard.Decisiveness.Value}}">{{.Scoreboard.Decisiveness.Value}}</td>
        <td class="score-{{.Scoreboard.Concreteness.Value}}">{{.Scoreboard.Concreteness.Value}}</td>
        <td><strong>{{.Scoreboard.Total}}</strong></td>
        <td>{{if .Scoreboard.CardsSchema}}<span class="ok">✓</span>{{else}}<span class="err">✗</span>{{end}}</td>
        <td>{{if .Scoreboard.ItalicBalanced}}<span class="ok">✓</span>{{else}}<span class="err">✗</span>{{end}}</td>
        <td>{{reactiveExpectSummary .ExpectHits}}</td>
        <td>{{durationMs .TotalLatency}}ms</td>
        <td>{{if .Degraded}}<span class="badge degraded">degraded</span>{{else if .AskErr}}<span class="err">error</span>{{else}}<span class="ok">ok</span>{{end}}</td>
      </tr>
      {{end}}
    </tbody>
  </table>

  {{range .ReactiveResults}}
  <div class="fixture" id="r-{{.FixtureName}}">
    <h2>{{.FixtureName}}</h2>
    <p><strong>Query:</strong> <em>{{.Query}}</em></p>
    <pre><strong>{{.Card.Title}}</strong> [{{.Card.Source}} / {{.Card.Rel}}]
{{.Card.Sub}}</pre>

    {{if or .Scoreboard.Calmness.Hits .Scoreboard.Decisiveness.Hits .Scoreboard.Concreteness.Hits}}
    <p><strong>Hits:</strong></p>
    <ul>
      {{range .Scoreboard.Calmness.Hits}}<li class="hits">calmness — {{.}}</li>{{end}}
      {{range .Scoreboard.Decisiveness.Hits}}<li class="hits">decisiveness — {{.}}</li>{{end}}
      {{range .Scoreboard.Concreteness.Hits}}<li class="hits">concreteness — {{.}}</li>{{end}}
    </ul>
    {{end}}

    {{if .AskErr}}<p class="err">ask error: {{.AskErr}}</p>{{end}}
  </div>
  {{end}}
  {{end}}
</body>
</html>
`

// WriteHTML renders the report to w.
func WriteHTML(w io.Writer, data ReportData) error {
	tmpl := template.New("report").Funcs(template.FuncMap{
		"durationMs": func(d time.Duration) int64 { return d.Milliseconds() },
		"reactiveExpectSummary": func(h ReactiveHits) template.HTML {
			labels := []struct {
				name string
				ok   bool
			}{
				{"title", h.Title},
				{"sub", h.Sub},
				{"src", h.Src},
				{"rel", h.Rel},
				{"not_deg", h.NotDeg},
			}
			passed := 0
			present, missing := []string{}, []string{}
			for _, l := range labels {
				if l.ok {
					passed++
					present = append(present, l.name)
				} else {
					missing = append(missing, l.name)
				}
			}
			parts := []string{fmt.Sprintf("<strong>%d/5</strong>", passed)}
			if len(present) > 0 {
				parts = append(parts, `<span class="ok">✓ `+strings.Join(present, ", ")+`</span>`)
			}
			if len(missing) > 0 {
				parts = append(parts, `<span class="err">✗ `+strings.Join(missing, ", ")+`</span>`)
			}
			return template.HTML(strings.Join(parts, " · "))
		},
		"joinList": func(xs []string) string {
			return strings.Join(xs, ", ")
		},
		"memoryGroundingSummary": func(m MemoryGrounding) template.HTML {
			if m.FactsInjected == 0 {
				return template.HTML("—")
			}
			parts := []string{fmt.Sprintf("<strong>%d/9</strong>", m.Total())}
			labels := []struct {
				name string
				val  int
			}{
				{"opener", m.OpenerTells.Value},
				{"density", m.FactDensity.Value},
				{"leak", m.VerbatimLeak.Value},
			}
			present, missing := []string{}, []string{}
			for _, l := range labels {
				if l.val >= 3 {
					present = append(present, l.name)
				} else {
					missing = append(missing, fmt.Sprintf("%s=%d", l.name, l.val))
				}
			}
			if len(present) > 0 {
				parts = append(parts, `<span class="ok">✓ `+strings.Join(present, ", ")+`</span>`)
			}
			if len(missing) > 0 {
				parts = append(parts, `<span class="err">✗ `+strings.Join(missing, ", ")+`</span>`)
			}
			return template.HTML(strings.Join(parts, " · "))
		},
		"stateMatchSummary": func(m StateMatch) template.HTML {
			if m.Expected == "" {
				return template.HTML("—")
			}
			label := template.HTMLEscapeString(string(m.Expected) + " → " + string(m.Actual))
			if m.OK {
				return template.HTML(`<span class="ok">✓ ` + label + `</span>`)
			}
			return template.HTML(`<span class="err">✗ ` + label + `</span>`)
		},
		"tensionBandSummary": func(t TensionInBand) template.HTML {
			if !t.Gated {
				// State unknown / empty → band wasn't applied. Show only
				// the raw tension so the column is never blank for a
				// rendered row.
				return template.HTML(fmt.Sprintf("tension=%d", t.Tension))
			}
			label := fmt.Sprintf("tension=%d in [%d,%d]", t.Tension, t.Low, t.High)
			escaped := template.HTMLEscapeString(label)
			if t.OK {
				return template.HTML(`<span class="ok">✓ ` + escaped + `</span>`)
			}
			return template.HTML(`<span class="err">✗ ` + escaped + `</span>`)
		},
		"mustCardsSummary": func(s Scoreboard) template.HTML {
			if len(s.MustCards) == 0 {
				return template.HTML("—")
			}
			passed := 0
			var present, missing []string
			for i, v := range s.MustCards {
				label := ""
				if i < len(s.MustCardLabels) {
					label = s.MustCardLabels[i]
				}
				if label == "" {
					label = fmt.Sprintf("#%d", i+1)
				}
				escaped := template.HTMLEscapeString(label)
				if v {
					passed++
					present = append(present, escaped)
				} else {
					missing = append(missing, escaped)
				}
			}
			parts := []string{fmt.Sprintf("<strong>%d/%d</strong>", passed, len(s.MustCards))}
			if len(present) > 0 {
				parts = append(parts, `<span class="ok">✓ `+strings.Join(present, ", ")+`</span>`)
			}
			if len(missing) > 0 {
				parts = append(parts, `<span class="err">✗ `+strings.Join(missing, ", ")+`</span>`)
			}
			return template.HTML(strings.Join(parts, " · "))
		},
	})
	parsed, err := tmpl.Parse(reportTemplate)
	if err != nil {
		return err
	}
	return parsed.Execute(w, data)
}

// SummaryLine is a one-line CLI summary per fixture, easy to skim.
func SummaryLine(r *RunResult) string {
	flags := []string{}
	if r.BriefingDeg {
		flags = append(flags, "DEGRADED")
	}
	if !r.Scoreboard.BriefingSchema {
		flags = append(flags, "BRIEFING-SCHEMA-FAIL")
	}
	if !r.Scoreboard.CardsSchema {
		flags = append(flags, "CARDS-SCHEMA-FAIL")
	}
	if !r.Scoreboard.AllMustCardsPresent() && len(r.Scoreboard.MustCards) > 0 {
		flags = append(flags, "MUST-CARDS-MISS")
	}
	flagStr := ""
	if len(flags) > 0 {
		flagStr = " · " + strings.Join(flags, " ")
	}
	mem := ""
	if r.Scoreboard.MemoryGrounding.FactsInjected > 0 {
		mem = fmt.Sprintf(" · mem=%d/9 (%d facts, %d refs, %d leaks)",
			r.Scoreboard.MemoryGrounding.Total(),
			r.Scoreboard.MemoryGrounding.FactsInjected,
			len(r.Scoreboard.MemoryGrounding.Subjects),
			len(r.Scoreboard.MemoryGrounding.VerbatimHits),
		)
	}
	return fmt.Sprintf(
		"[%s] calm=%d decisive=%d concrete=%d total=%d/9 latency=%dms%s%s",
		r.FixtureName,
		r.Scoreboard.Calmness.Value,
		r.Scoreboard.Decisiveness.Value,
		r.Scoreboard.Concreteness.Value,
		r.Scoreboard.Total(),
		r.TotalLatency.Milliseconds(),
		mem,
		flagStr,
	)
}
