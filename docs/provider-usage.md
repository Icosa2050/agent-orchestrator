# API-billed usage (fork)

Fork-only (Icosa2050, see [`FORK.md`](../FORK.md) "Usage seams"). AO estimates
the cost of API-billed DeepSeek work at read time from the tokens it already
records, using the effective-dated DeepSeek price book
(`backend/internal/forkusage/pricebook`). Nothing is stored: a new price
version applies to the next read, including history.

## Commands

| Command | What it prints |
|---|---|
| `ao usage prices [--check deepseek] [--accept <sha>] [--json]`, `ao usage prices auto on\|off` | the price book: versions, rates, last page check |
| `ao usage window [--at <time>] [--next offpeak\|peak] [--json]` | the DeepSeek peak/off-peak window now or at any instant |
| `ao usage report [--days N] [--offset N] [--tz Z] [-p P] [--by session] [--top N] [--fleet]` | the upstream usage report (JSON) with the DeepSeek extension merged under `ext` |
| `ao usage report --by hour [--days N] [--tz Z] [--json] [--fleet]` | DeepSeek usage per UTC hour of day, peak hours marked |
| `ao usage limits [--json] [--fleet]` | the upstream limits, the provider rows (when polling is on) and the steering block (below) |
| `ao usage providers enable\|disable deepseek\|openrouter`, `ao usage providers status [--json]` | opt-in balance / key polling (default off) |
| `ao usage providers set runwayLowDays=3 balanceLow=5 keyRemainingLowPct=25 modelsListing=on\|off` | steering thresholds and the OpenRouter model-listing switch (local only) |

## How a DeepSeek reply is priced

- **Route.** `deepseek-account` (DeepSeek Harness platform login),
  `deepseek-official` and its alias `deepseek` (API key) draw from the same
  platform wallet at API prices, so all three are priced the same way. Any
  other route is `unknown-route`: tokens shown, cost unknown.
- **Model.** The stored model id is kept as `model`. Retired names resolve
  through the version's aliases (`deepseek-v4-flash` is billed as
  `deepseek-flash` from the 2026-09-10 version on) and the resolved name is
  `billedAs`. An id the version does not list is `unknown-model`; a row stored
  without a model has `modelMissing: true` and `unknown-model`. Nothing is
  guessed by model family.
- **Price version.** Each reply uses the version whose effective range
  contains its window instant. Replies before the first evidenced version are
  `no-price-evidence` (tokens shown, cost unknown). A report row never spans
  two versions.
- **Window instant** (our rule; DeepSeek does not specify one): the reply
  time, except that a request whose start lies in the other window than its
  reply is priced by its request start. The DeepSeek parser records the
  request start (`requestStartedAt` in the event's provider usage JSON) from
  the last `request/context` frame before the reply, so retries use the
  attempt that produced the reply. Rows ingested before that change have no
  request start: a reply landing within 5 minutes after a window edge is
  priced by reply time and counted in `boundaryTokens`, with
  `windowBasis.reply-time-unverified`.
- **Peak/off-peak.** Peak is Mon-Fri 01:00-04:00 and 06:00-10:00 UTC,
  except Chinese public holidays (Beijing dates). Make-up working weekends are
  off-peak (literal reading of the price page, flagged as an assumption). A
  weekday in a year without a transcribed holiday calendar is priced peak (an
  upper bound) and warned about.
- **Math.** tokens x rate per component (cache hit, cache miss, output),
  each rounded half-up to nano-USD; totals are sums of rounded components.

## `ao usage report` merge contract

The CLI reads `GET /api/v1/usage/report` exactly as before, then
`GET /api/v1/usage/fork/report-ext?days&offset&tz&projectId` from the same
host.

- Upstream keys stay as the daemon sent them. Each row (period mode) or
  session row (`--by session`) with DeepSeek events gains an `ext` object:
  `billedAs`, `routes`, `priceVersions`, `billingStatuses`, `events`,
  `pricedEvents`, `pricebookCostNanos`, `peakListCostNanos`,
  `catalogCostNanos`, `replacedCatalogCostNanos`, `effectiveCostNanos`,
  `boundaryTokens` (and `windowBasis` in period mode).
  `effectiveCostNanos` = the row's upstream `costNanos` minus the catalog
  estimate of the events the price book priced, plus their price-book cost,
  so events the price book did not price (another route, an unknown model)
  keep their catalog cost.
- The report gains a top-level `ext`: the report-ext answer with
  `status: "ok"`. It carries `pricedRows[]` (per period, harness, model,
  speed, route, price version and billing status, with `peak` and `offPeak`
  buckets), `sessionRows[]`, `sessionModels{sessionId: [...]}` (every model a
  session used, most tokens first, with `costBasis` `pricebook`, `catalog`,
  `mixed` (price book for the DeepSeek events it priced, catalog for the rest)
  or `none`), `hours[]`, `totals`, `peakRule`, `priceVersions`,
  `windowRule`, `boundaryRule`, `priceBook` and `warnings`.
- Rows are ranked by `ext.effectiveCostNanos` where they have priced DeepSeek
  events and by upstream `costNanos` otherwise: session rows always, period
  rows with `--top N`. A DeepSeek account-route row the upstream catalog
  leaves unpriced therefore still ranks. With `--top` the CLI re-reads the
  upstream report without `top` and truncates itself; `ext.rankedBy` says
  so. `--fleet` does the same per host, every read going to that host.
- The merge replaces catalog costs with price-book costs, so both reads must
  describe the same events. report-ext reads its DeepSeek events and the
  upstream report events in one read transaction and returns `snapshot`:
  every upstream row key (period rows and session rows) with the event count
  and `costNanos` the upstream report shows over that snapshot. The CLI merges
  only when the report it read has the same window and every row matches the
  snapshot (no row missing on either side; with a `top`-truncated report only
  the rows it has are compared). Otherwise (ingestion or a day/hour rollover
  between the reads) it re-reads both, the report first, up to 3 times, and
  then reports `ext.status: "error"` with the upstream answer unchanged.
- A daemon without report-ext (HTTP 404): `ext: {"status": "unsupported"}`.
  Any other failure: `ext: {"status": "error", "reason": ...}`. Neither fails
  the command.
- `catalogCostNanos` is upstream's stored estimate (the LiteLLM catalog,
  peak-only and blind to the account route); `pricebookCostNanos` is the
  windowed read-time price; `peakListCostNanos` prices the same replies at
  peak list rates. The desktop Usage page still shows the catalog cost.

## `ao usage report --by hour`

24 buckets, one per UTC hour of the day, summed over the window (default 7
days; the window is in UTC whatever `--tz` says). Each bucket has events,
cache-hit, cache-miss and output tokens, the windowed cost and its peak and
off-peak parts. `peakHour` marks hours inside the weekday peak ranges;
replies in those hours on weekends and holidays are still off-peak, which the
peak/off-peak split shows. A reply is bucketed by its window instant (see
above). `--tz` only adds a local-time column (`display` in JSON). Every label
uses one offset, the zone's offset at the window's last instant
(`displayOffsetAt`, `displayUtcOffset`), even when a DST switch falls inside
the window or on that date. `--json` gives:

```json
{"by":"hour","provider":"deepseek","days":7,"since":"...","until":"...",
 "windowsTimeZone":"UTC","displayTimeZone":"Europe/Berlin","displayOffsetAt":"...","displayUtcOffset":"+01:00",
 "windowRule":"...","boundaryRule":"...","peakRule":{"version":"...","weekdays":["Mon","Tue","Wed","Thu","Fri"],"utcRanges":[["01:00","04:00"],["06:00","10:00"]],"excludeHolidays":"cn"},
 "priceVersions":["deepseek-2026-09-10"],
 "hours":[{"hour":0,"utc":"00:00","display":"01:00","peakHour":false,"events":12,
           "cacheHitTokens":0,"cacheMissTokens":0,"outputTokens":0,
           "peak":{"events":0,"cacheHitTokens":0,"cacheMissTokens":0,"outputTokens":0,"costNanos":0},
           "offPeak":{"events":12,"...":"..."},
           "pricedEvents":12,"pricebookCostNanos":120000000,"peakListCostNanos":240000000,
           "catalogCostNanos":null,"catalogPricedEvents":0}, "... 24 entries ..."],
 "totals":{"events":0,"...":"..."},"priceBook":{"version":"...","source":"embedded","stale":false,"warnings":[]},"warnings":[]}
```

The keys above are stable (schema 1); new keys may be added.

## Steering (`ao usage limits`)

AO supplies signals and advice; orchestrators decide. Nothing is blocked or
throttled. `ao usage limits --json` carries the key `steering`:

```json
{"steering":{"schema":1,
  "deepseek":{"keyId":"a1b2c3d4e5f6","window":"peak","priceFactor":1,"peakVsOffPeak":2,
    "windowEndsAt":"2026-11-25T10:00:00Z","windowEndsAtBerlin":"2026-11-25T11:00:00+01:00",
    "nextOffPeak":{"from":"…","until":"…","fromBerlin":"…","untilBerlin":"…"},"nextPeak":{…},
    "models":[{"model":"deepseek-flash","nowPerM":{…},"peakPerM":{…},"offPeakPerM":{…}}],
    "alsoVia":{"route":"openrouter","currency":"USD","note":"information only: …","models":[{"id":"deepseek/deepseek-v4.1-flash","promptPerM":"0.0507","completionPerM":"0.2028"}]},
    "balance":{"amount":"42.10","currency":"USD","isAvailable":true,"source":"user/balance","fetchedAt":"…"},
    "runway":{"days":12.3,"avgDailySpend":"3.42","currency":"USD","basis":"balance change, last 7 d, adjustment intervals excluded","coverageDays":6.6},
    "advice":{"code":"defer-nonurgent","reason":"peak until Wed 10:00 UTC (11:00 Berlin); off-peak is half price"},
    "priceBook":{"version":"deepseek-2026-11-24","source":"embedded","stale":false}},
  "openrouter":{"keyId":"9f8e7d6c5b4a","timePricing":"none known",
    "key":{"remainingUsd":73.2,"limitUsd":100,"remainingPct":73.2,"limitBasis":"key limit","resetInterval":"weekly","resetsAt":"…","resetsAtBerlin":"…"},
    "runway":{"days":16.5,"avgDailySpend":"4.43","currency":"USD","basis":"usage_weekly / elapsed UTC week days (at least 1)"},
    "models":[{"model":"deepseek/deepseek-v4.1-flash","status":"listed","promptPerM":"0.0507","completionPerM":"0.2028","cacheReadPerM":"0.001014","blendedPerM":"0.08112","rank":1}],
    "presets":[{"model":"@preset/pal-cheap","name":"pal-cheap","chain":[{"model":"deepseek/deepseek-v4.1-flash","status":"listed","blendedPerM":"0.08112"}],"source":"PAL model registry description (OPENROUTER_MODELS_CONFIG_PATH)"}],
    "blendBasis":"fallback 4:1 input:output (no PAL ledger yet)",
    "allowedModelsSource":"~/.claude.json mcpServers.pal.env.OPENROUTER_ALLOWED_MODELS",
    "listingStatus":"ok","listedAt":"…","listingSource":"openrouter.ai/api/v1/models (public, keyless)",
    "advice":{"code":"ok","reason":"$73.20 of $100.00 left (weekly); resets Mon 00:00 UTC (01:00 Berlin)"}},
  "thresholds":{"runwayLowDays":3,"balanceLow":"5","keyRemainingLowPct":25,"preferCheaperPct":50}}}
```

The text form follows the provider rows:

```
steer: deepseek DEFER-NONURGENT (peak until Wed 10:00 UTC (11:00 Berlin); off-peak is half price; off-peak until Thu 01:00 UTC (02:00 Berlin); runway 12.3 d)
       openrouter OK ($73.20 of $100.00 left (weekly); resets Mon 00:00 UTC (01:00 Berlin); runway 16.5 d; cheapest allowed: deepseek/deepseek-v4.1-flash $0.08/M blended)
```

With `--fleet`, `steering` holds lists (`deepseek[]`, `openrouter[]`), one
entry per distinct key with the `hosts` that report it, plus `warnings` when
hosts use different DeepSeek price books.

### Advice codes

| Provider | Code | When | Meaning for the orchestrator |
|---|---|---|---|
| deepseek | `stop` | `is_available=false` or balance ≤ 0 | start no DeepSeek work; tell the owner |
| deepseek | `low-runway` | runway < `runwayLowDays` (3) or balance < `balanceLow` (5, account currency) | ask the owner before non-urgent work; overrides the window |
| deepseek | `defer-nonurgent` | peak now | defer non-urgent work to `nextOffPeak.from`; if it must run, prefer flash and fewer reviewers |
| deepseek | `ok-offpeak` | off-peak now | run deferred work; `windowEndsAt` says how long it stays cheap |
| deepseek | `unknown` | price book stale or missing, holiday year missing (window still shown) | treat as peak |
| openrouter | `stop` | binding limit's `remainingUsd` ≤ 0 (a zero limit counts as exhausted) | no PAL calls; tell the owner |
| openrouter | `low-runway` | runway < `runwayLowDays` or remaining < `keyRemainingLowPct` (25 %) | ask before non-urgent PAL use; use the rank-1 model |
| openrouter | `prefer-cheaper` | remaining < 50 % | choose cheaper allowed models when quality allows |
| openrouter | `ok` | otherwise (also: a key without a limit) | — |
| openrouter | `unknown` | key polling off, the key never read, or the last refresh failed and the last reading would be `ok` | treat the budget as tight |

Precedence: `stop` > `low-runway` > window / limit codes. `unknown` never
means "go". Thresholds compare unrounded values; `days` and `remainingPct`
are rounded for display only. Without a DeepSeek balance (polling off or
failing) the window codes still apply and the reason says why the balance is
unknown. Each entry carries the row's `status` (`ok`, `disabled`,
`unavailable`) and `statusReason`; after a failed refresh the last good
reading is used and marked (`balance.stale`, `openrouter.stale`), and its
cautionary codes stay, but OpenRouter never answers `ok` on it. The §9.2
OpenRouter `stop` on a 402 in the PAL ledger arrives with the ledger reader
(PR E). `runway.avgDailySpend` is a decimal string with `runway.currency`
for both providers (USD for OpenRouter).

- **Runway.** DeepSeek: balance / average daily spend over the last 7 days,
  from the balance history (`deepseek-balance.jsonl`) of the same key and
  currency; intervals with a top-up or grant are excluded. `null` with a
  reason when polling is off, with under 2 days of covered history, or when no
  spend was observed. AO-session spend from the report extension is never
  used: both DeepSeek routes and tools outside AO share the wallet, so it
  would overstate the runway. OpenRouter: remaining of the binding limit (the
  effective budget or the key limit, whichever has the smaller share left)
  / (`usage_weekly` / UTC week days elapsed at the reading, at least 1).
  With `--fleet`, the newest reading of a shared key stays authoritative;
  without its own runway it borrows another host's average daily spend
  against its own balance, which can only tighten the advice.
- **Model prices.** `GET https://openrouter.ai/api/v1/models` is public and
  keyless. The daemon fetches it at most once a day (retry after a failure:
  1 h; attempts persist across restarts), with no `Authorization` header, no
  cookies and redirects refused, and caches the price fields in
  `provider-usage/openrouter-models.json`. Default **on**;
  `ao usage providers set modelsListing=off` stops it (the cached listing is
  still shown, marked).
- **Allowed models** come from `mcpServers.pal.env.OPENROUTER_ALLOWED_MODELS`
  in `~/.claude.json` (names only). `~` router aliases are looked up directly
  in the listing; a model missing there is `not listed`. `@preset/...` entries
  are not priced; each is listed under `presets` with its chain as PAL's own
  model registry (`OPENROUTER_MODELS_CONFIG_PATH`) describes it
  ("OpenRouter preset pal-cheap: a -> b -> c"), informational only, since
  OpenRouter resolves presets server-side.
- **Ranking.** `blendedPerM` = (4 × prompt + 1 × completion) / 5 per million
  tokens, the fallback mix until the PAL ledger (PR E) supplies an observed
  one. It is a ranking aid; the raw listed prices are always shown. The
  listing is OpenRouter's displayed price; the charge can differ.
- **Cross-route.** `deepseek.alsoVia` lists OpenRouter's `deepseek/*` prices
  next to the direct API prices, as information only: moving DeepSeek work to
  OpenRouter changes the serving host and its data policy, an owner decision.
- **Deferral.** There is no AO deferral mechanism in v1. Orchestrators use
  `ao usage window --next offpeak` and their own timer.

### Orchestrator rule text

Paste into project orchestrator rules:

```
## API spend steering (DeepSeek, PAL/OpenRouter)
- Before dispatching DeepSeek work (DeepSeek Harness sessions, dsh reviews, panels, bulk runs) or PAL-heavy work, read `ao usage limits --json` -> `steering`. Do not poll it; read it once per dispatch decision.
- deepseek `stop`: start no DeepSeek work; tell the owner. `low-runway`: stop or ask the owner before non-urgent work.
- deepseek `defer-nonurgent` (peak, 2x price): defer non-urgent work (reviews that do not block a merge, second opinions, panels, bulk/backfill) to `steering.deepseek.nextOffPeak.from` (`ao usage window --next offpeak`). If it must run now: use deepseek-flash instead of deepseek-v4-pro, one reviewer instead of several, smaller context.
- deepseek `ok-offpeak`: run deferred DeepSeek work now; check `windowEndsAt` so long batches finish before peak.
- Urgent work (blocking a merge, CI recovery, owner request) is never deferred for price; note the peak cost in your report.
- PAL/OpenRouter: prefer the cheapest allowed model that is good enough for the task (`steering.openrouter.models`, rank 1 first); for PAL prefer `pal-cheap` or `pal-balanced` when quality allows and keep `pal-strong` and expensive models (by `blendedPerM`) for hard problems. `prefer-cheaper`/`low-runway`: cheaper models only, ask before large consensus runs. `stop`: no PAL calls.
- `unknown` advice means "assume peak / assume tight budget", never "go". `ok` means no budget concern, not "use the strongest model".
- Report deferrals in your progress report ("deferred 3 DeepSeek reviews to off-peak 10:00 UTC").
```

## Reference numbers

The golden fixture `backend/internal/forkusage/reportext/testdata/deepseek-30d-sample.csv`
is an anonymized real 30-day sample (2,140 DeepSeek Harness replies, reply
times, routes, models and token counts only). Priced with windows it costs
$21.67; at peak-only rates $36.03; about 82 % of its tokens ran off-peak.
