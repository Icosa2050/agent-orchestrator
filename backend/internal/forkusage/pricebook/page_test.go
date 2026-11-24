package pricebook

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

const realPeakNote = "(2) Off-peak rates are half of the peak rates. Peak hours are 01:00 - 04:00 and 06:00 - 10:00 UTC, Monday through Friday, excluding Chinese public holidays. All other hours are off-peak, including weekends and Chinese public holidays in full."

const realAliasNote = "(1) Use `deepseek-flash` as the model name. The legacy names `deepseek-v4-flash` and `deepseek-v4-flash-vision-exp` are still accepted, but the corresponding models have been retired, their requests are served by the DeepSeek-V4.1-Flash-1017 model and billed at the Flash price."

type pageModel struct {
	header        string
	peak, offPeak [3]string // hit, miss, output, with currency symbol
}

func flashCol() pageModel {
	return pageModel{"deepseek-flash(1)", [3]string{"$0.006", "$0.3", "$1.2"}, [3]string{"$0.003", "$0.15", "$0.6"}}
}

func proCol() pageModel {
	return pageModel{"deepseek-v4-pro", [3]string{"$0.044", "$1.32", "$3.96"}, [3]string{"$0.022", "$0.66", "$1.98"}}
}

// mkPage renders a page in the layout of the real pricing page.
func mkPage(models []pageModel, notes ...string) string {
	var b strings.Builder
	b.WriteString("# Models & Pricing\n\n## Model Details\n\n| MODEL | MODEL | MODEL |")
	for _, m := range models {
		b.WriteString(" " + m.header + " |")
	}
	b.WriteString("\n| --- | --- | --- |" + strings.Repeat(" --- |", len(models)) + "\n")
	b.WriteString("| BASE URL | BASE URL | BASE URL |" + strings.Repeat(" <https://api.deepseek.com> |", len(models)) + "\n")
	labels := []string{"1M INPUT TOKENS / (CACHE HIT)", "1M INPUT TOKENS / (CACHE MISS)", "1M OUTPUT TOKENS"}
	for i, label := range labels {
		for _, w := range []string{"OFF-PEAK", "PEAK"} {
			fmt.Fprintf(&b, "| PRICING(2) | %s | %s |", label, w)
			for _, m := range models {
				v := m.peak[i]
				if w == "OFF-PEAK" {
					v = m.offPeak[i]
				}
				b.WriteString(" " + v + " |")
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
	for _, n := range notes {
		b.WriteString(n + "\n\n")
	}
	return b.String()
}

func TestParseRealPage(t *testing.T) {
	body, err := os.ReadFile("testdata/pricing-2026-11-24.md")
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParsePage(body)
	if err != nil {
		t.Fatalf("parse real page: %v", err)
	}
	if p.Currency != "USD" || !p.StatedHalf || len(p.Models) != 2 {
		t.Fatalf("page: %+v", p)
	}
	if got := p.Models["deepseek-v4-pro"]; got.Peak != (Rates{"0.044", "1.32", "3.96"}) || got.OffPeak != (Rates{"0.022", "0.66", "1.98"}) {
		t.Errorf("pro: %+v", got)
	}
	if got := p.Models["deepseek-flash"]; got.Peak != (Rates{"0.006", "0.3", "1.2"}) || got.OffPeak != (Rates{"0.003", "0.15", "0.6"}) {
		t.Errorf("flash: %+v", got)
	}
	if fmt.Sprint(p.Peak.Weekdays) != "[Mon Tue Wed Thu Fri]" || fmt.Sprint(p.Peak.UTCRanges) != "[[01:00 04:00] [06:00 10:00]]" || p.Peak.ExcludeHolidays != "cn" {
		t.Errorf("peak rule: %+v", p.Peak)
	}
	if p.Aliases["deepseek-v4-flash"] != "deepseek-flash" || p.Aliases["deepseek-v4-flash-vision-exp"] != "deepseek-flash" || len(p.Aliases) != 2 {
		t.Errorf("aliases: %+v", p.Aliases)
	}
	// The real page matches the embedded latest version exactly.
	d, cand, err := Compare(mustBook(t).Latest(), p, ts(t, "2026-11-24T15:00:00Z"), "sha")
	if err != nil || d.Outcome != OutcomeUnchanged || cand != nil {
		t.Errorf("real page vs embedded: %+v %v %v", d, cand, err)
	}
}

func TestParseRejectsBrokenPages(t *testing.T) {
	good := mkPage([]pageModel{flashCol(), proCol()}, realAliasNote, realPeakNote)
	if _, err := ParsePage([]byte(good)); err != nil {
		t.Fatalf("synthetic page rejected: %v", err)
	}
	fixture, _ := os.ReadFile("testdata/pricing-2026-11-24.md")
	for name, body := range map[string]string{
		"html":             "<!DOCTYPE html><html><body>" + good + "</body></html>",
		"truncated":        good[:len(good)/2],
		"empty":            "",
		"no peak note":     mkPage([]pageModel{flashCol(), proCol()}, realAliasNote),
		"no weekdays":      mkPage([]pageModel{flashCol(), proCol()}, realAliasNote, strings.Replace(realPeakNote, "Monday through Friday", "on working days", 1)),
		"no UTC":           mkPage([]pageModel{flashCol(), proCol()}, realAliasNote, strings.Replace(realPeakNote, "UTC", "Beijing time", 1)),
		"unknown holidays": mkPage([]pageModel{flashCol(), proCol()}, realAliasNote, strings.Replace(realPeakNote, "excluding Chinese public holidays", "excluding some holidays", 1)),
		"text price":       strings.Replace(good, "$0.044", "contact sales", 1),
		"price range":      strings.Replace(good, "$0.044", "$0.04-0.05", 1),
		"alias note gone":  mkPage([]pageModel{flashCol(), proCol()}, realPeakNote),
		"mixed currency":   strings.Replace(good, "$0.044", "¥0.3", 1),
		"bad model header": strings.Replace(good, "deepseek-v4-pro", "DeepSeek V4 Pro", 1),
		"layout reshaped":  strings.ReplaceAll(string(fixture), "| PRICING(2)", "| COST(2)"),
	} {
		if _, err := ParsePage([]byte(body)); err == nil {
			t.Errorf("%s: parsed", name)
		} else if pe := (*ParseError)(nil); !errors.As(err, &pe) {
			t.Errorf("%s: error %T is not a ParseError", name, err)
		}
	}
	// A page in yuan parses with its currency, so the safety rail flags it.
	allCNY := mkPage([]pageModel{
		{"deepseek-flash(1)", [3]string{"¥0.04", "¥2", "¥8"}, [3]string{"¥0.02", "¥1", "¥4"}},
		{"deepseek-v4-pro", [3]string{"¥0.3", "¥9", "¥27"}, [3]string{"¥0.15", "¥4.5", "¥13.5"}},
	}, realAliasNote, realPeakNote)
	p, err := ParsePage([]byte(allCNY))
	if err != nil || p.Currency != "CNY" {
		t.Fatalf("CNY page: %+v %v", p, err)
	}
	d, _, _ := Compare(mustBook(t).Latest(), p, time.Now(), "x")
	if d.Outcome != OutcomeSuspicious {
		t.Errorf("CNY page outcome: %+v", d)
	}
}

func TestCompareChanges(t *testing.T) {
	latest := mustBook(t).Latest()
	at := ts(t, "2026-12-03T06:12:00Z")
	compare := func(body string) (Diff, *Version) {
		t.Helper()
		p, err := ParsePage([]byte(body))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		d, cand, err := Compare(latest, p, at, "sha1")
		if err != nil {
			t.Fatal(err)
		}
		return d, cand
	}
	// One price changed.
	pro := proCol()
	pro.peak[1], pro.offPeak[1] = "$1.1", "$0.55"
	d, cand := compare(mkPage([]pageModel{flashCol(), pro}, realAliasNote, realPeakNote))
	if d.Outcome != OutcomeChanged || cand == nil || cand.Models["deepseek-v4-pro"].PeakPerM.CacheMiss != "1.1" ||
		!cand.EffectiveFrom.Equal(at) || cand.Label != AutoLabel || cand.Source != SourcePageAuto || cand.PageSHA256 != "sha1" {
		t.Fatalf("price change: %+v %+v", d, cand)
	}
	if len(d.ModelChanges) != 1 || d.ModelChanges[0].Model != "deepseek-v4-pro" {
		t.Errorf("model changes: %+v", d.ModelChanges)
	}
	// A model vanished: kept and marked retiredOnPage (one of two is not more than half).
	d, cand = compare(mkPage([]pageModel{flashCol()}, realAliasNote, realPeakNote))
	if d.Outcome != OutcomeChanged || !cand.Models["deepseek-v4-pro"].RetiredOnPage || cand.Models["deepseek-v4-pro"].PeakPerM.Output != "3.96" {
		t.Errorf("removed model: %+v %+v", d, cand)
	}
	// A new model.
	d, cand = compare(mkPage([]pageModel{flashCol(), proCol(), {"deepseek-v5", [3]string{"$0.1", "$1", "$4"}, [3]string{"$0.05", "$0.5", "$2"}}}, realAliasNote, realPeakNote))
	if d.Outcome != OutcomeChanged || len(d.NewModels) != 1 || cand.Models["deepseek-v5"].PeakPerM.Output != "4" {
		t.Errorf("new model: %+v", d)
	}
	// Peak hours changed.
	d, cand = compare(mkPage([]pageModel{flashCol(), proCol()}, realAliasNote, strings.Replace(realPeakNote, "06:00 - 10:00", "06:00 - 11:00", 1)))
	if d.Outcome != OutcomeChanged || fmt.Sprint(cand.Peak.UTCRanges) != "[[01:00 04:00] [06:00 11:00]]" || !containsString(d.Changes, "peak hours changed") {
		t.Errorf("peak hours: %+v %+v", d, cand)
	}
	// Off-peak no longer exactly half: explicit offPeakPerM.
	flash := flashCol()
	flash.offPeak[2] = "$0.5"
	d, cand = compare(mkPage([]pageModel{flash, proCol()}, realAliasNote, realPeakNote))
	if d.Outcome != OutcomeChanged || cand.Models["deepseek-flash"].OffPeakPerM == nil || cand.Models["deepseek-flash"].OffPeakPerM.Output != "0.5" ||
		cand.Models["deepseek-v4-pro"].OffPeakPerM != nil {
		t.Errorf("explicit off-peak: %+v %+v", d, cand)
	}
}

func TestCompareSafetyRail(t *testing.T) {
	latest := mustBook(t).Latest()
	for name, models := range map[string][]pageModel{
		"x10 up":   {flashCol(), {"deepseek-v4-pro", [3]string{"$0.5", "$1.32", "$3.96"}, [3]string{"$0.25", "$0.66", "$1.98"}}},
		"x10 down": {{"deepseek-flash(1)", [3]string{"$0.006", "$0.02", "$1.2"}, [3]string{"$0.003", "$0.01", "$0.6"}}, proCol()},
		"vanished": {{"deepseek-v5", [3]string{"$0.1", "$1", "$4"}, [3]string{"$0.05", "$0.5", "$2"}}},
	} {
		p, err := ParsePage([]byte(mkPage(models, realAliasNote, realPeakNote)))
		if name == "vanished" {
			p, err = ParsePage([]byte(mkPage(models, realPeakNote)))
		}
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		d, cand, err := Compare(latest, p, time.Now(), "s")
		if err != nil || d.Outcome != OutcomeSuspicious || cand == nil || len(d.Suspicious) == 0 {
			t.Errorf("%s: %+v %v", name, d, err)
		}
	}
	// Exactly ×10 is not suspicious.
	p, _ := ParsePage([]byte(mkPage([]pageModel{flashCol(), {"deepseek-v4-pro", [3]string{"$0.44", "$1.32", "$3.96"}, [3]string{"$0.22", "$0.66", "$1.98"}}}, realAliasNote, realPeakNote)))
	if d, _, _ := Compare(latest, p, time.Now(), "s"); d.Outcome != OutcomeChanged {
		t.Errorf("x10 exactly: %+v", d)
	}
}

// Review regressions (Codex): off-peak-only jumps, currency-only changes and
// models that reappear after being retired.
func TestCompareReviewRegressions(t *testing.T) {
	latest := mustBook(t).Latest()
	parse := func(body string) *Page {
		t.Helper()
		p, err := ParsePage([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	// Off-peak only, x100: suspicious even though the peak price is unchanged.
	pro := proCol()
	pro.offPeak[2] = "$198"
	d, cand, _ := Compare(latest, parse(mkPage([]pageModel{flashCol(), pro}, realAliasNote, realPeakNote)), time.Now(), "s")
	if d.Outcome != OutcomeSuspicious || cand == nil {
		t.Errorf("off-peak jump: %+v", d)
	}
	// Same numbers in yuan: never "unchanged".
	yuan := strings.ReplaceAll(mkPage([]pageModel{flashCol(), proCol()}, realAliasNote, realPeakNote), "$", "¥")
	d, cand, _ = Compare(latest, parse(yuan), time.Now(), "s")
	if d.Outcome != OutcomeSuspicious || cand == nil || d.Currency != "CNY" || len(d.Changes) == 0 {
		t.Errorf("currency-only change: %+v", d)
	}
	// Retired, then listed again with new rates: the new rates win.
	retired := latest
	retired.Models = map[string]ModelPrice{
		"deepseek-flash":  latest.Models["deepseek-flash"],
		"deepseek-v4-pro": {PeakPerM: latest.Models["deepseek-v4-pro"].PeakPerM, RetiredOnPage: true},
	}
	back := proCol()
	back.peak, back.offPeak = [3]string{"$0.04", "$1.2", "$3.6"}, [3]string{"$0.02", "$0.6", "$1.8"}
	d, cand, _ = Compare(retired, parse(mkPage([]pageModel{flashCol(), back}, realAliasNote, realPeakNote)), time.Now(), "s")
	if d.Outcome != OutcomeChanged || cand.Models["deepseek-v4-pro"].RetiredOnPage || cand.Models["deepseek-v4-pro"].PeakPerM.Output != "3.6" {
		t.Errorf("reappearing model: %+v %+v", d, cand)
	}
}

func TestWindowRatiosOnlyWhenUniform(t *testing.T) {
	latest := mustBook(t).Latest()
	odd := latest
	odd.Version, odd.Source, odd.Label = "deepseek-auto-2026-12-03", SourcePageAuto, AutoLabel
	odd.EffectiveFrom = ts(t, "2026-12-03T00:00:00Z")
	odd.Models = map[string]ModelPrice{
		"deepseek-flash":  {PeakPerM: Rates{"0.006", "0.3", "1.2"}, OffPeakPerM: &Rates{"0.003", "0.15", "0.4"}},
		"deepseek-v4-pro": latest.Models["deepseek-v4-pro"],
	}
	b, err := Load([]Version{odd})
	if err != nil {
		t.Fatal(err)
	}
	if w := b.WindowAt(ts(t, "2026-12-05T12:00:00Z")); w.Window != WindowOffPeak || w.PriceFactor != "" {
		t.Errorf("non-uniform off-peak factor: %+v", w)
	}
	if w := mustBook(t).WindowAt(ts(t, "2026-11-28T12:00:00Z")); w.PriceFactor != "0.5" {
		t.Errorf("uniform factor: %+v", w)
	}
}

// Review regression (PR #40): a model the page drops and lists as a legacy
// alias of a new model is a rename, not an invalid version.
func TestCompareModelRenamedToAlias(t *testing.T) {
	b := mustBook(t)
	latest := b.Latest()
	renameNote := "(3) Use `deepseek-pro` as the model name. The legacy name `deepseek-v4-pro` is still accepted, its requests are served by the new model and billed at the Pro price."
	pro := pageModel{"deepseek-pro(3)", [3]string{"$0.04", "$1.2", "$3.6"}, [3]string{"$0.02", "$0.6", "$1.8"}}
	p, err := ParsePage([]byte(mkPage([]pageModel{flashCol(), pro}, realAliasNote, realPeakNote, renameNote)))
	if err != nil {
		t.Fatal(err)
	}
	if p.Aliases["deepseek-v4-pro"] != "deepseek-pro" {
		t.Fatalf("aliases: %+v", p.Aliases)
	}
	at := ts(t, "2026-12-03T06:12:00Z")
	d, cand, err := Compare(latest, p, at, "sha")
	if err != nil || d.Outcome != OutcomeChanged || cand == nil {
		t.Fatalf("rename: %+v %v %v", d, cand, err)
	}
	if _, kept := cand.Models["deepseek-v4-pro"]; kept || cand.Aliases["deepseek-v4-pro"] != "deepseek-pro" || len(d.Removed) != 0 {
		t.Errorf("candidate: models %v aliases %v removed %v", cand.Models, cand.Aliases, d.Removed)
	}
	if !containsString(d.Changes, "model deepseek-v4-pro now billed as deepseek-pro") {
		t.Errorf("changes: %v", d.Changes)
	}
	// Recorded, the old name prices at the new model's rate from detection;
	// earlier events keep the previous version's Pro price.
	cand.Version = "deepseek-auto-2026-12-03"
	nb, err := Load([]Version{*cand})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := nb.Price(Usage{Route: "deepseek", Model: "deepseek-v4-pro", At: at.Add(-time.Second), OutputTokens: 1_000_000})
	after, _ := nb.Price(Usage{Route: "deepseek", Model: "deepseek-v4-pro", At: at, OutputTokens: 1_000_000})
	if before.BilledAs != "deepseek-v4-pro" || before.Window != WindowPeak || before.TotalNanos != 3_960_000_000 {
		t.Errorf("before: %+v", before)
	}
	if after.BilledAs != "deepseek-pro" || after.TotalNanos != 3_600_000_000 {
		t.Errorf("after: %+v", after)
	}

	// The renamed model is itself an alias target: deepseek-v4-flash ->
	// deepseek-flash, and the page renames deepseek-flash to deepseek-flash-2
	// without mentioning deepseek-v4-flash. The old alias follows the rename.
	flashRename := "(1) Use `deepseek-flash-2` as the model name. The legacy name `deepseek-flash` is still accepted and billed at the Flash 2 price."
	flash2 := pageModel{"deepseek-flash-2(1)", [3]string{"$0.005", "$0.25", "$1"}, [3]string{"$0.0025", "$0.125", "$0.5"}}
	p, err = ParsePage([]byte(mkPage([]pageModel{flash2, proCol()}, flashRename, realPeakNote)))
	if err != nil {
		t.Fatal(err)
	}
	d, cand, err = Compare(latest, p, at, "sha2")
	if err != nil || d.Outcome != OutcomeChanged || cand == nil {
		t.Fatalf("flash rename: %+v %v %v", d, cand, err)
	}
	for _, alias := range []string{"deepseek-flash", "deepseek-v4-flash", "deepseek-v4-flash-vision-exp"} {
		if cand.Aliases[alias] != "deepseek-flash-2" {
			t.Errorf("alias %s -> %q, want deepseek-flash-2 (aliases %v)", alias, cand.Aliases[alias], cand.Aliases)
		}
	}
	if !containsString(d.Changes, "legacy name deepseek-v4-flash follows deepseek-flash to deepseek-flash-2") {
		t.Errorf("changes: %v", d.Changes)
	}
	cand.Version = "deepseek-auto-2026-12-03"
	nb, err = Load([]Version{*cand})
	if err != nil {
		t.Fatal(err)
	}
	legacy, _ := nb.Price(Usage{Route: "deepseek", Model: "deepseek-v4-flash", At: at, OutputTokens: 1_000_000})
	if legacy.Status != StatusPriced || legacy.BilledAs != "deepseek-flash-2" || legacy.TotalNanos != 1_000_000_000 {
		t.Errorf("legacy alias after rename: %+v", legacy)
	}
}
