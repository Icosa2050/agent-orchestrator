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
  `catalogCostNanos`, `boundaryTokens` (and `windowBasis` in period mode).
- The report gains a top-level `ext`: the report-ext answer with
  `status: "ok"`. It carries `pricedRows[]` (per period, harness, model,
  speed, route, price version and billing status, with `peak` and `offPeak`
  buckets), `sessionRows[]`, `sessionModels{sessionId: [...]}` (every model a
  session used, most tokens first, with `costBasis` `pricebook`, `catalog`
  or `none`), `hours[]`, `totals`, `peakRule`, `priceVersions`,
  `windowRule`, `boundaryRule`, `priceBook` and `warnings`.
- `--top N` ranks rows by `pricebookCostNanos` where a row has priced DeepSeek
  events and by upstream `costNanos` otherwise, so a DeepSeek account-route row
  the upstream catalog leaves unpriced still ranks. To do that the CLI re-reads
  the upstream report without `top` and truncates itself; `ext.rankedBy`
  says so. Under `--fleet` the ranking stays upstream's.
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
above). `--tz` only adds a local-time column (`display` in JSON), using the
zone's offset at the end of the window. `--json` gives:

```json
{"by":"hour","provider":"deepseek","days":7,"since":"...","until":"...",
 "windowsTimeZone":"UTC","displayTimeZone":"Europe/Berlin","displayOffsetAt":"...",
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

## Reference numbers

The golden fixture `backend/internal/forkusage/reportext/testdata/deepseek-30d-sample.csv`
is an anonymized real 30-day sample (2,140 DeepSeek Harness replies, reply
times, routes, models and token counts only). Priced with windows it costs
$21.67; at peak-only rates $36.03; about 82 % of its tokens ran off-peak.
