package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kshitijv97/between-us/backend/internal/analysis"
	"github.com/kshitijv97/between-us/backend/internal/analytics"
)

// metricsUsage documents `betweenus metrics`.
const metricsUsage = `Usage: betweenus metrics [flags] <chat id>

Shows the metrics of a chat for one period (docs/04-metrics-catalog.md).
Examples:
  betweenus metrics <id>                              the chat's latest month
  betweenus metrics -window year -anchor 2024-06-01 <id>
  betweenus metrics -window custom -from 2024-01-01 -to 2024-03-31 <id>

Flags:
`

// metricsCmd implements `betweenus metrics`.
func metricsCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("metrics", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, metricsUsage); fs.PrintDefaults() }
	window := fs.String("window", "month", "day, week, month, year, all or custom")
	anchor := fs.String("anchor", "", "a date (YYYY-MM-DD) inside the period; default: the chat's last day")
	from := fs.String("from", "", "first day for -window custom (YYYY-MM-DD)")
	to := fs.String("to", "", "last day for -window custom, inclusive (YYYY-MM-DD)")
	compare := fs.Bool("compare", true, "compare with the previous period")
	asJSON := fs.Bool("json", false, "print the full API response as JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return exitUsage
	}
	chatID, err := uuid.Parse(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, "invalid chat id:", err)
		return exitUsage
	}
	q := analytics.Query{Kind: analytics.WindowKind(*window), Compare: *compare}
	if *anchor != "" {
		if q.Anchor, err = analytics.ParseDate(*anchor); err != nil {
			fmt.Fprintln(stderr, "-anchor:", err)
			return exitUsage
		}
	}
	if q.Kind == "custom" {
		f, err1 := analytics.ParseDate(*from)
		t, err2 := analytics.ParseDate(*to)
		if err1 != nil || err2 != nil {
			fmt.Fprintln(stderr, "-window custom needs -from and -to as YYYY-MM-DD")
			return exitUsage
		}
		q.From, q.To = f, t.AddDays(1)
	}

	ctx := context.Background()
	a, err := openApp(ctx, stderr, true)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	defer a.close()
	if ctx, err = a.localScope(ctx); err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	svc := &analysis.Service{Store: a.store, LexiconDir: a.cfg.LexiconDir, Log: a.log}
	res, err := svc.Metrics(ctx, chatID, q)
	if err != nil {
		fmt.Fprintln(stderr, "metrics:", err)
		return exitError
	}
	if *asJSON {
		return printJSON(stdout, res)
	}
	printMetricsSummary(stdout, res)
	return exitOK
}

// analyzeCmd implements `betweenus analyze <chat id>`: recompute derived data.
func analyzeCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: betweenus analyze <chat id>")
		return exitUsage
	}
	chatID, err := uuid.Parse(args[0])
	if err != nil {
		fmt.Fprintln(stderr, "invalid chat id:", err)
		return exitUsage
	}
	ctx := context.Background()
	a, err := openApp(ctx, stderr, true)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	defer a.close()
	if ctx, err = a.localScope(ctx); err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	svc := &analysis.Service{Store: a.store, LexiconDir: a.cfg.LexiconDir, Log: a.log}
	sum, err := svc.Analyze(ctx, chatID)
	if err != nil {
		fmt.Fprintln(stderr, "analyze:", err)
		return exitError
	}
	fmt.Fprintf(stdout, "Analysed %d messages: %d conversations, %d daily rows in %s\n",
		sum.Messages, sum.Sessions, sum.Rollups, sum.Duration.Round(time.Millisecond))
	return exitOK
}

// printMetricsSummary renders the headline metrics for people. It works on
// the generic JSON form of the result so it stays decoupled from the exact
// Go result types; the full detail is available with -json.
func printMetricsSummary(w io.Writer, res *analytics.Result) {
	raw, err := json.Marshal(res)
	if err != nil {
		fmt.Fprintln(w, "cannot render metrics:", err)
		return
	}
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	labels := map[string]string{}
	if ps, ok := doc["participants"].([]any); ok {
		for _, p := range ps {
			if pm, ok := p.(map[string]any); ok {
				labels[fmt.Sprint(pm["id"])] = fmt.Sprint(pm["label"])
			}
		}
	}
	if win, ok := doc["window"].(map[string]any); ok {
		to, _ := analytics.ParseDate(fmt.Sprint(win["to"]))
		fmt.Fprintf(w, "Period       %v %s → %s (%v days, %v)\n", win["kind"], win["from"], to.AddDays(-1), win["days"], win["tz"])
	}
	metrics, _ := doc["metrics"].(map[string]any)
	// row prints one metric: per-participant values from m[mapKey] (each
	// value or value[field]) followed by the combined value.
	row := func(name, id, mapKey, field string, format func(any) string) {
		m, ok := metrics[id].(map[string]any)
		if !ok {
			return
		}
		var parts []string
		if bp, ok := m[mapKey].(map[string]any); ok {
			for pid, v := range bp {
				parts = append(parts, fmt.Sprintf("%s %s", labels[pid], format(pick(v, field))))
			}
		}
		if c, ok := m["combined"]; ok && c != nil {
			parts = append(parts, "total "+format(pick(c, field)))
		}
		if len(parts) > 0 {
			fmt.Fprintf(w, "%-12s %s\n", name, strings.Join(sortedStable(parts), " · "))
		}
	}
	row("Messages", "M-001", "by_participant", "", num)
	row("Initiations", "M-008", "by_participant", "", num)
	row("Reply time", "M-010", "by_participant", "median_s", dur)
	row("Late night", "M-014", "share", "", pct)
	row("Sentiment", "M-022", "by_participant", "mean", dec)
	for _, id := range []string{"M-025", "M-026"} {
		m, ok := metrics[id].(map[string]any)
		if !ok {
			continue
		}
		name := map[string]string{"M-025": "Balance", "M-026": "Engagement"}[id]
		if v, ok := m["value"]; ok && v != nil {
			fmt.Fprintf(w, "%-12s %s / 100\n", name, num(v))
		} else {
			fmt.Fprintf(w, "%-12s not enough messages in this period\n", name)
		}
	}
	fmt.Fprintln(w, "\nFull detail for all 27 metrics: add -json")
}

// pick returns v[field] for objects, or v itself for scalars.
func pick(v any, field string) any {
	if field == "" {
		return v
	}
	if m, ok := v.(map[string]any); ok {
		return m[field]
	}
	return v
}

func num(v any) string {
	if f, ok := v.(float64); ok {
		return fmt.Sprintf("%.0f", f)
	}
	return "–"
}

func dec(v any) string {
	if f, ok := v.(float64); ok {
		return fmt.Sprintf("%+.2f", f)
	}
	return "–"
}

func pct(v any) string {
	if f, ok := v.(float64); ok {
		return fmt.Sprintf("%.0f%%", f*100)
	}
	return "–"
}

func dur(v any) string {
	f, ok := v.(float64)
	if !ok || math.IsNaN(f) {
		return "–"
	}
	return (time.Duration(f) * time.Second).Round(time.Second).String()
}

// sortedStable keeps "total" last and participants in label order.
func sortedStable(parts []string) []string {
	out := make([]string, 0, len(parts))
	var total string
	for _, p := range parts {
		if strings.HasPrefix(p, "total ") {
			total = p
			continue
		}
		out = append(out, p)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	if total != "" {
		out = append(out, total)
	}
	return out
}
