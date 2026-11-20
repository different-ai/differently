package acid

import (
	"fmt"
	"io"
	"slices"
	"strings"
)

// Symbols used in reports, as in docs/acid-test.md.
var symbols = map[Outcome]string{Pass: "✅", Unsupported: "➖", Fail: "❌", Dishonest: "⚠️", Skipped: "⏭"}

// Symbol returns an outcome's report symbol.
func Symbol(o Outcome) string { return symbols[o] }

// Headline is "Level 1 · 12/21" for a report.
func (r Report) Headline() string {
	level := "no level"
	if r.Level >= 0 {
		level = fmt.Sprintf("Level %d", r.Level)
	}
	return fmt.Sprintf("%s · %d/%d", level, r.Passed, r.Total)
}

// WriteMarkdown writes one run as Markdown: the headline, then every test.
func WriteMarkdown(w io.Writer, r Report) {
	name := r.Pool
	if name == "" {
		name = r.Adapter.Kind
	}
	fmt.Fprintf(w, "## %s (%s): %s\n\n", name, r.Adapter.Kind, r.Headline())
	fmt.Fprintf(w, "%s, %s, %.0fs. Isolation: %s. Features: %s.\n\n", r.Suite, r.StartedAt.Format("2006-01-02 15:04 MST"), r.Seconds,
		orDash(r.Adapter.Isolation), orDash(strings.Join(r.Features, ", ")))
	if len(r.Leaked) > 0 {
		fmt.Fprintf(w, "**Leaked instances:** %s\n\n", strings.Join(r.Leaked, ", "))
	}
	fmt.Fprintln(w, "| ID | Test | Outcome | Evidence | Time |")
	fmt.Fprintln(w, "|---|---|---|---|---:|")
	for _, res := range r.Results {
		fmt.Fprintf(w, "| %s | %s | %s %s | %s | %.1fs |\n", res.ID, res.Title, Symbol(res.Outcome), res.Outcome, cell(res.Evidence), res.Seconds)
	}
	var metrics []string
	for _, res := range r.Results {
		keys := make([]string, 0, len(res.Metrics))
		for k := range res.Metrics {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			metrics = append(metrics, fmt.Sprintf("| %s | %s | %.3f |", res.ID, k, res.Metrics[k]))
		}
	}
	if len(metrics) > 0 {
		fmt.Fprintln(w, "\n| Test | Measurement | Value |\n|---|---|---:|")
		fmt.Fprintln(w, strings.Join(metrics, "\n"))
	}
}

// WriteMatrix writes several runs side by side, one column per run.
func WriteMatrix(w io.Writer, reports []Report) {
	fmt.Fprint(w, "| ID | Test |")
	for _, r := range reports {
		name := r.Pool
		if name == "" {
			name = r.Adapter.Kind
		}
		fmt.Fprintf(w, " %s |", name)
	}
	fmt.Fprint(w, "\n|---|---|")
	for range reports {
		fmt.Fprint(w, ":---:|")
	}
	fmt.Fprint(w, "\n| | **Score** |")
	for _, r := range reports {
		fmt.Fprintf(w, " **%s** |", r.Headline())
	}
	fmt.Fprintln(w)
	for _, t := range Tests() {
		row := fmt.Sprintf("| %s | %s |", t.ID, t.Title)
		any := false
		for _, r := range reports {
			i := slices.IndexFunc(r.Results, func(x Result) bool { return x.ID == t.ID })
			if i < 0 {
				row += " |"
				continue
			}
			any = true
			row += " " + Symbol(r.Results[i].Outcome) + " |"
		}
		if any {
			fmt.Fprintln(w, row)
		}
	}
}

func cell(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	return strings.ReplaceAll(s, "\n", " ")
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
