package pricebook

import (
	"math/big"
	"strings"
	"testing"
	"time"
)

func mustBook(t *testing.T) *Book {
	t.Helper()
	b, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("load embedded: %v", err)
	}
	return b
}

func ts(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestEmbeddedDataIsValid(t *testing.T) {
	b := mustBook(t)
	vs := b.Versions()
	if len(vs) != 2 || vs[0].Version != "deepseek-2026-08-16" || vs[1].Version != "deepseek-2026-09-10" {
		t.Fatalf("versions = %+v", vs)
	}
	for _, v := range vs {
		if v.Evidence == "" || v.CheckedAt == "" || v.Source != SourceEmbedded {
			t.Errorf("version %s lacks evidence/checkedAt/source", v.Version)
		}
	}
	years := b.Calendar().Years()
	if len(years) < 1 || years[0].Year != 2026 {
		t.Fatalf("calendar years = %+v", years)
	}
	for _, y := range years {
		if len(y.Holidays) < 7 {
			t.Errorf("year %d has %d blocks", y.Year, len(y.Holidays))
		}
	}
	// Every model id observed in AO's DeepSeek usage resolves today.
	for _, model := range []string{"deepseek-v4-pro", "deepseek-flash", "deepseek-v4-flash", "deepseek-v4-flash-vision-exp"} {
		p, err := b.Price(Usage{Route: "deepseek-account", Model: model, At: ts(t, "2026-11-20T12:00:00Z"), OutputTokens: 1})
		if err != nil || p.Status != StatusPriced {
			t.Errorf("model %s: %+v %v", model, p, err)
		}
	}
}

func TestStrictDecodingRejectsBadData(t *testing.T) {
	cal := mustBook(t).Calendar()
	good := `{"schemaVersion":1,"provider":"deepseek","currency":"USD","sourceUrl":"x","routes":{"deepseek":"api"},"versions":[` +
		`{"version":"v1","effectiveFrom":"2026-01-01T00:00:00Z","source":"embedded","models":{"m":{"peakPerM":{"cacheHit":"0.1","cacheMiss":"0.2","output":"0.3"}}},` +
		`"aliases":{},"offPeakMultiplier":"0.5","peak":{"weekdays":["Mon"],"utcRanges":[["01:00","04:00"]],"excludeHolidays":"cn"}}]}`
	if _, err := build([]byte(good), nil, cal); err != nil {
		t.Fatalf("good data rejected: %v", err)
	}
	for name, bad := range map[string]string{
		"unknown field":   strings.Replace(good, `"sourceUrl"`, `"extra":1,"sourceUrl"`, 1),
		"exponent":        strings.Replace(good, `"0.1"`, `"1e-1"`, 1),
		"negative":        strings.Replace(good, `"0.1"`, `"-0.1"`, 1),
		"empty price":     strings.Replace(good, `"0.1"`, `""`, 1),
		"currency":        strings.Replace(good, `"USD"`, `"CNY"`, 1),
		"route billing":   strings.Replace(good, `"api"`, `"plan"`, 1),
		"weekday":         strings.Replace(good, `"Mon"`, `"Monday"`, 1),
		"reversed range":  strings.Replace(good, `["01:00","04:00"]`, `["04:00","01:00"]`, 1),
		"bad clock":       strings.Replace(good, `"01:00"`, `"1:00"`, 1),
		"multiplier > 1":  strings.Replace(good, `"0.5"`, `"2"`, 1),
		"dangling alias":  strings.Replace(good, `"aliases":{}`, `"aliases":{"a":"nope"}`, 1),
		"holiday kind":    strings.Replace(good, `"cn"`, `"us"`, 1),
		"trailing data":   good + `{}`,
		"source mismatch": strings.Replace(good, `"embedded"`, `"page"`, 1),
	} {
		if _, err := build([]byte(bad), nil, cal); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCalendarValidation(t *testing.T) {
	base := `{"schemaVersion":1,"dayBoundary":"x","years":[{"year":2026,"notice":"n","sourceUrl":"u","publishedAt":"p","transcribedAt":"t","holidays":[` +
		`{"name":"a","from":"2026-01-01","to":"2026-01-03"},{"name":"b","from":"2026-02-15","to":"2026-02-23"},{"name":"c","from":"2026-04-04","to":"2026-04-06"},` +
		`{"name":"d","from":"2026-05-01","to":"2026-05-05"},{"name":"e","from":"2026-06-19","to":"2026-06-21"},{"name":"f","from":"2026-09-25","to":"2026-09-27"},` +
		`{"name":"g","from":"2026-10-01","to":"2026-10-07"}],"makeUpWorkdays":["2026-01-04"]}]}`
	if _, err := ParseCalendar([]byte(base)); err != nil {
		t.Fatalf("base rejected: %v", err)
	}
	for name, bad := range map[string]string{
		"weekday make-up":  strings.Replace(base, `["2026-01-04"]`, `["2026-01-05"]`, 1),
		"holiday make-up":  strings.Replace(base, `["2026-01-04"]`, `["2026-01-03"]`, 1),
		"too few blocks":   strings.Replace(base, `,{"name":"g","from":"2026-10-01","to":"2026-10-07"}`, ``, 1),
		"overlap":          strings.Replace(base, `"2026-06-19","to":"2026-06-21"`, `"2026-05-05","to":"2026-05-06"`, 1),
		"bad date":         strings.Replace(base, `"2026-01-01"`, `"2026-13-01"`, 1),
		"unknown field":    strings.Replace(base, `"dayBoundary"`, `"x":1,"dayBoundary"`, 1),
		"reversed holiday": strings.Replace(base, `"from":"2026-01-01","to":"2026-01-03"`, `"from":"2026-01-03","to":"2026-01-01"`, 1),
	} {
		if _, err := ParseCalendar([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestWindowUTCEdges(t *testing.T) {
	b := mustBook(t)
	// Tuesday 2026-11-24, an ordinary working day.
	for _, c := range []struct {
		at   string
		peak bool
	}{
		{"2026-11-24T00:59:59Z", false}, {"2026-11-24T01:00:00Z", true},
		{"2026-11-24T03:59:59Z", true}, {"2026-11-24T04:00:00Z", false},
		{"2026-11-24T05:59:59Z", false}, {"2026-11-24T06:00:00Z", true},
		{"2026-11-24T09:59:59Z", true}, {"2026-11-24T10:00:00Z", false},
		{"2026-11-27T09:59:00Z", true},                                   // Friday
		{"2026-11-28T02:00:00Z", false}, {"2026-11-29T07:00:00Z", false}, // weekend
		{"2026-11-30T00:59:59Z", false}, {"2026-11-30T01:00:00Z", true}, // Monday
	} {
		if got := b.IsPeak(ts(t, c.at)); got != c.peak {
			t.Errorf("%s: peak = %v, want %v", c.at, got, c.peak)
		}
	}
}

func TestWindowSameInstantAnyZone(t *testing.T) {
	b := mustBook(t)
	at := ts(t, "2026-11-24T06:30:00Z")
	for _, name := range []string{"UTC", "Europe/Berlin", "Asia/Shanghai", "America/Los_Angeles", "Pacific/Kiritimati"} {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Fatal(err)
		}
		w := b.WindowAt(at.In(loc))
		if w.Window != WindowPeak || !w.Until.Equal(ts(t, "2026-11-24T10:00:00Z")) {
			t.Errorf("%s: %+v", name, w)
		}
	}
}

func TestWindowHolidays(t *testing.T) {
	b := mustBook(t)
	// National Day, Thursday 2026-10-01 is a holiday weekday: off-peak.
	w := b.WindowAt(ts(t, "2026-10-01T02:00:00Z"))
	if w.Window != WindowOffPeak || w.Reason != ReasonHoliday || w.Holiday == "" {
		t.Fatalf("holiday: %+v", w)
	}
	// Beijing-date boundary: 2026-10-07 is the last holiday; Beijing 10-08
	// starts at 2026-10-07T16:00Z.
	if k, _ := b.Calendar().Day(ts(t, "2026-10-07T15:59:59Z")); k != DayHoliday {
		t.Errorf("10-07 15:59Z: %v", k)
	}
	if k, _ := b.Calendar().Day(ts(t, "2026-10-07T16:00:00Z")); k != DayNormal {
		t.Errorf("10-07 16:00Z: %v", k)
	}
	if !b.IsPeak(ts(t, "2026-10-08T01:00:00Z")) {
		t.Error("first working day after National Day is not peak")
	}
	// Off-peak from Wed 30 Sep 10:00 runs through the holiday to Thu 8 Oct 01:00.
	w = b.WindowAt(ts(t, "2026-09-30T12:00:00Z"))
	if !w.Since.Equal(ts(t, "2026-09-30T10:00:00Z")) || !w.Until.Equal(ts(t, "2026-10-08T01:00:00Z")) {
		t.Errorf("holiday stretch: since %v until %v", w.Since, w.Until)
	}
	// A make-up working Saturday stays off-peak and is flagged.
	w = b.WindowAt(ts(t, "2026-10-10T02:00:00Z"))
	if w.Window != WindowOffPeak || w.Reason != ReasonMakeUpWorkday || !containsString(w.Assumptions, AssumptionMakeUpDays) {
		t.Errorf("make-up Saturday: %+v", w)
	}
	// 2027 is transcribed: Spring Festival Monday 2027-02-08 is off-peak.
	if b.IsPeak(ts(t, "2027-02-08T02:00:00Z")) {
		t.Error("2027 Spring Festival weekday priced as peak")
	}
}

func TestWindowMissingHolidayYear(t *testing.T) {
	b := mustBook(t)
	// Wednesday 2028-01-05: no 2028 notice transcribed.
	w := b.WindowAt(ts(t, "2028-01-05T02:00:00Z"))
	if w.Window != WindowPeak || !containsString(w.Assumptions, AssumptionYearMissed) || !strings.Contains(w.HolidayCalendar, "2028 missing") {
		t.Errorf("missing year peak: %+v", w)
	}
	p, err := b.Price(Usage{Route: "deepseek", Model: "deepseek-flash", At: ts(t, "2028-01-05T02:00:00Z"), OutputTokens: 1000})
	if err != nil || !p.HolidayYearMissing || p.Window != WindowPeak {
		t.Errorf("missing year price: %+v %v", p, err)
	}
	if b.IsPeak(ts(t, "2028-01-05T12:00:00Z")) {
		t.Error("missing year 12:00 is peak")
	}
}

func TestWindowNextSpans(t *testing.T) {
	b := mustBook(t)
	// Friday 09:59 UTC: peak until 10:00, off-peak through the weekend.
	w := b.WindowAt(ts(t, "2026-11-27T09:59:00Z"))
	if w.Window != WindowPeak || !w.Until.Equal(ts(t, "2026-11-27T10:00:00Z")) {
		t.Fatalf("friday: %+v", w)
	}
	if w.NextOffPeak == nil || !w.NextOffPeak.From.Equal(ts(t, "2026-11-27T10:00:00Z")) || !w.NextOffPeak.Until.Equal(ts(t, "2026-11-30T01:00:00Z")) {
		t.Errorf("friday next off-peak: %+v", w.NextOffPeak)
	}
	if w.NextPeak == nil || !w.NextPeak.From.Equal(ts(t, "2026-11-30T01:00:00Z")) || !w.NextPeak.Until.Equal(ts(t, "2026-11-30T04:00:00Z")) {
		t.Errorf("friday next peak: %+v", w.NextPeak)
	}
	// Sunday: next peak is Monday 01:00.
	if got, _ := b.NextStart(ts(t, "2026-11-29T20:00:00Z"), true); !got.Equal(ts(t, "2026-11-30T01:00:00Z")) {
		t.Errorf("sunday next peak = %v", got)
	}
	// Already off-peak: the next off-peak start is now.
	now := ts(t, "2026-11-24T05:00:00Z")
	if got, _ := b.NextStart(now, false); !got.Equal(now) {
		t.Errorf("off-peak next off-peak = %v", got)
	}
	// Between the two morning ranges the off-peak gap is one hour.
	w = b.WindowAt(now)
	if !w.Since.Equal(ts(t, "2026-11-24T04:00:00Z")) || !w.Until.Equal(ts(t, "2026-11-24T06:00:00Z")) {
		t.Errorf("gap: %+v", w)
	}
}

func TestBerlinDisplayAcrossDST(t *testing.T) {
	b := mustBook(t)
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	// The window is pure UTC; Berlin wall time shifts by an hour at DST.
	winter := b.WindowAt(ts(t, "2026-03-27T07:00:00Z")) // Friday before the March switch (CET)
	summer := b.WindowAt(ts(t, "2026-03-30T07:00:00Z")) // Monday after (CEST)
	if winter.Until.In(berlin).Format("15:04") != "11:00" || summer.Until.In(berlin).Format("15:04") != "12:00" {
		t.Errorf("berlin until: winter %s summer %s", winter.Until.In(berlin), summer.Until.In(berlin))
	}
	october := b.WindowAt(ts(t, "2026-10-26T02:00:00Z")) // Monday after the October switch (CET)
	if october.Until.In(berlin).Format("15:04") != "05:00" {
		t.Errorf("october until: %s", october.Until.In(berlin))
	}
}

func TestEffectiveDatesAndAliases(t *testing.T) {
	b := mustBook(t)
	price := func(at, model string) Priced {
		t.Helper()
		p, err := b.Price(Usage{Route: "deepseek-official", Model: model, At: ts(t, at), CacheHitTokens: 1_000_000})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if p := price("2026-08-16T15:59:59Z", "deepseek-v4-pro"); p.Status != StatusNoPriceEvidence || p.TotalNanos != 0 {
		t.Errorf("before first version: %+v", p)
	}
	// Exactly at effectiveFrom: the first version, and its every-day rule
	// prices a Sunday at peak.
	if p := price("2026-08-16T16:00:00Z", "deepseek-v4-pro"); p.Status != StatusPriced || p.Version != "deepseek-2026-08-16" {
		t.Errorf("at effectiveFrom: %+v", p)
	}
	if p := price("2026-08-23T02:00:00Z", "deepseek-v4-pro"); p.Window != WindowPeak || p.TotalNanos != 44_000_000 {
		t.Errorf("v1 sunday peak: %+v", p)
	}
	// The legacy name is its own model in v1 and an alias of deepseek-flash in v2.
	if p := price("2026-09-01T02:00:00Z", "deepseek-v4-flash"); p.BilledAs != "deepseek-v4-flash" || p.TotalNanos != 14_000_000 {
		t.Errorf("v1 legacy flash: %+v", p)
	}
	if p := price("2026-09-10T03:59:59Z", "deepseek-flash"); p.Status != StatusUnknownModel {
		t.Errorf("flash before its version: %+v", p)
	}
	if p := price("2026-09-01T02:00:00Z", "deepseek-v4-flash-vision-exp"); p.Status != StatusUnknownModel {
		t.Errorf("unpriced vision-exp in v1: %+v", p)
	}
	if p := price("2026-09-10T04:00:00Z", "deepseek-v4-flash"); p.BilledAs != "deepseek-flash" || p.Version != "deepseek-2026-09-10" {
		t.Errorf("v2 alias: %+v", p)
	}
	if p := price("2026-11-24T12:00:00Z", "deepseek-v4.1-flash"); p.Status != StatusUnknownModel {
		t.Errorf("unknown future slug: %+v", p)
	}
	for _, route := range []string{"deepseek-account", "deepseek-official", "deepseek"} {
		p, _ := b.Price(Usage{Route: route, Model: "deepseek-flash", At: ts(t, "2026-11-24T12:00:00Z"), OutputTokens: 1})
		if p.Status != StatusPriced {
			t.Errorf("route %s: %+v", route, p)
		}
	}
	if p, _ := b.Price(Usage{Route: "openrouter", Model: "deepseek-flash", At: ts(t, "2026-11-24T12:00:00Z")}); p.Status != StatusUnknownRoute {
		t.Errorf("foreign route: %+v", p)
	}
}

func TestPriceMath(t *testing.T) {
	b := mustBook(t)
	u := Usage{Route: "deepseek-account", Model: "deepseek-v4-pro", CacheHitTokens: 1_234_567, CacheMissTokens: 89_012, OutputTokens: 3_456}
	u.At = ts(t, "2026-11-24T02:00:00Z")
	peak, err := b.Price(u)
	if err != nil {
		t.Fatal(err)
	}
	// 0.044, 1.32, 3.96 USD/M = 44, 1320, 3960 nano-USD per token.
	if peak.CacheHitNanos != 1_234_567*44 || peak.CacheMissNanos != 89_012*1320 || peak.OutputNanos != 3_456*3960 {
		t.Fatalf("peak components: %+v", peak)
	}
	if peak.TotalNanos != peak.CacheHitNanos+peak.CacheMissNanos+peak.OutputNanos {
		t.Fatalf("total is not the sum of components: %+v", peak)
	}
	u.At = ts(t, "2026-11-24T12:00:00Z")
	off, _ := b.Price(u)
	if off.Window != WindowOffPeak || off.TotalNanos*2 != peak.TotalNanos {
		t.Errorf("off-peak is not half: %+v vs %+v", off, peak)
	}
	for _, at := range []string{"2026-11-24T02:00:00Z", "2026-11-24T12:00:00Z"} {
		rates, ok := b.Rates(ts(t, at))
		if !ok {
			t.Fatal("no rates")
		}
		for _, r := range rates {
			for i, pair := range [][2]string{{r.PeakPerM.CacheHit, r.OffPeakPerM.CacheHit}, {r.PeakPerM.CacheMiss, r.OffPeakPerM.CacheMiss}, {r.PeakPerM.Output, r.OffPeakPerM.Output}} {
				p, _ := new(big.Rat).SetString(pair[0])
				o, _ := new(big.Rat).SetString(pair[1])
				if new(big.Rat).Mul(o, big.NewRat(2, 1)).Cmp(p) != 0 {
					t.Errorf("%s component %d: off-peak %s is not half of %s", r.Model, i, pair[1], pair[0])
				}
			}
		}
	}
	if _, err := b.Price(Usage{Route: "deepseek", Model: "deepseek-flash", At: u.At, OutputTokens: -1}); err == nil {
		t.Error("negative tokens accepted")
	}
}

func TestComponentRoundingHalfUp(t *testing.T) {
	for _, c := range []struct {
		tokens int64
		perM   string
		want   int64
	}{
		{1, "0.0005", 1},  // 0.5 nano -> 1
		{3, "0.0005", 2},  // 1.5 -> 2
		{1, "0.0004", 0},  // 0.4 -> 0
		{7, "0.00015", 1}, // 1.05 -> 1
		{0, "3.96", 0},
		{1_000_000, "1.32", 1_320_000_000},
	} {
		r, _ := new(big.Rat).SetString(c.perM)
		got, err := componentNanos(c.tokens, r)
		if err != nil || got != c.want {
			t.Errorf("%d × %s = %d (%v), want %d", c.tokens, c.perM, got, err, c.want)
		}
	}
	// Totals are sums of rounded components, so rows add up exactly.
	r, _ := new(big.Rat).SetString("0.0005")
	a, _ := componentNanos(1, r)
	bb, _ := componentNanos(1, r)
	if a+bb != 2 {
		t.Errorf("rounded components do not add: %d + %d", a, bb)
	}
}

func TestAutoVersionsMergeAndDedupe(t *testing.T) {
	latest := mustBook(t).Latest()
	same := latest
	same.Version, same.Source, same.Label = "deepseek-auto-2026-11-25", SourcePageAuto, AutoLabel
	same.EffectiveFrom = ts(t, "2026-11-25T06:00:00Z")
	changed := same
	changed.Version = "deepseek-auto-2026-12-03"
	changed.EffectiveFrom = ts(t, "2026-12-03T06:12:00Z")
	changed.Models = map[string]ModelPrice{
		"deepseek-flash":  {PeakPerM: Rates{CacheHit: "0.005", CacheMiss: "0.25", Output: "1"}},
		"deepseek-v4-pro": latest.Models["deepseek-v4-pro"],
	}
	b, err := Load([]Version{same, changed})
	if err != nil {
		t.Fatal(err)
	}
	vs := b.Versions()
	if len(vs) != 3 || vs[2].Version != "deepseek-auto-2026-12-03" {
		t.Fatalf("merged versions: %v", vs)
	}
	before, _ := b.Price(Usage{Route: "deepseek", Model: "deepseek-flash", At: ts(t, "2026-12-03T06:11:59Z"), OutputTokens: 1_000_000})
	after, _ := b.Price(Usage{Route: "deepseek", Model: "deepseek-flash", At: ts(t, "2026-12-03T06:12:00Z"), OutputTokens: 1_000_000})
	if before.Version != "deepseek-2026-09-10" || before.TotalNanos != 1_200_000_000 || after.Version != "deepseek-auto-2026-12-03" || after.TotalNanos != 1_000_000_000 {
		t.Errorf("auto version selection: before %+v after %+v", before, after)
	}
	dup := changed
	dup.Version = "deepseek-2026-09-10"
	if _, err := Load([]Version{dup}); err == nil {
		t.Error("duplicate version name accepted")
	}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
