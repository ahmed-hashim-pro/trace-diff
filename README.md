# trace-diff

[![CI](https://github.com/ahmed-hashim-pro/trace-diff/actions/workflows/ci.yml/badge.svg)](https://github.com/ahmed-hashim-pro/trace-diff/actions/workflows/ci.yml) [![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Compares OpenTelemetry traces from before and after a change, and reports
the structural regressions that tests and latency dashboards miss: a query
that now runs once per item, a call that is now retried, a fan-out that
grew, a new call the request has to wait for. Each finding names the
operation, says how often and how much, and points at trace ids you can open.

A change can pass every test, leave the latency graph flat or even better,
and still double the work behind each request. On a laptop, fifty queries
against a local database take a millisecond and a half, so nothing in the
latency moves. In production, with the database a network hop away, the
same code costs fifty round trips per page. A retry that hides a slow
dependency improves p95 while the dependency does the slow work twice. The
traces show both; trace-diff reads them for you.

```
$ trace-diff check examples/shop/before.jsonl.gz examples/shop/after.jsonl.gz
before: examples/shop/before.jsonl.gz, 300 traces, 2128 spans
after:  examples/shop/after.jsonl.gz, 300 traces, 4203 spans

endpoint                   requests     median ms        p95 ms
frontend GET /orders/{id}  300 → 300    9.69 → 9.52      73.4 → 50.3

FAIL  n-plus-one  frontend GET /orders/{id}
      repeated per request: orders · db · SELECT id, sku, qty, price_cents FROM order_items WHERE id = ?
      new: in 100% of requests, up to 50 calls each (median 5, p95 9), one after another
      replaces SELECT id, sku, qty, price_cents FROM order_items WHERE order_id = ? ORDER BY id (1 call per request before, gone after)
      called from orders · server · GET /orders/{id}
      the request waits on it in 100% of those requests: 0.13 ms median, 0.37 ms p95
      trace after  08f7b4dbb263f116221d9c87ef3ef9b2  50 calls
      trace after  d322190dec4fa55e9a8508da05719035  typical, 5 calls
      trace before 223fb023e8f229999614a3f2e1d7706c  median request, no such call

FAIL  retry-storm  frontend GET /orders/{id}
      retries: frontend · client · GET pricing:8080 /quote/{n} → pricing GET /quote/{order}
      requests with a retry: 0% before, 10% after
      attempts per request where called: mean 1.00 before, 1.11 after (max 3)
      called from frontend · server · GET /orders/{id}
      the request waits on it in 100% of those requests: 9.00 ms median, 49.7 ms p95
      trace after  29974935e470596f305343263f6851dc  3 attempts, 2 retries
      trace after  ab6f33fa44c095debb23d0f5fb7b26d7  typical, 2 attempts
      trace before 223fb023e8f229999614a3f2e1d7706c  median request, 1 call

FAIL  new-sync-call  frontend GET /orders/{id}
      new call: orders · db · SELECT id FROM order_items WHERE order_id = ? ORDER BY id
      ...

3 failing, 0 to look at, 0 for context
$ echo $?
1
```

The full report is [`docs/report.txt`](docs/report.txt). The same code run
twice is reported as no change ([`docs/rerun.txt`](docs/rerun.txt)).

![Two waterfalls on one time scale. Before: the frontend asks orders for the order, orders runs two queries, then pricing answers in about 9 ms. After: orders runs a query for the item ids, then one query per item, drawn in red; six are shown and a dotted bar stands for 44 more like them](docs/n-plus-one.svg)

![Two waterfalls on one time scale. Before: one call to pricing, about 9 ms. After: the frontend's first call to pricing gives up at 40 ms with an error, the second also times out, the third succeeds; under each failed attempt the pricing server keeps working on the abandoned request, past the end of the page](docs/retry-storm.svg)

![Terminal recording: trace-diff check on the same code run twice reports no structural changes; then on the before and after runs it reports the N+1, the retry storm and the new query, and exits 1](docs/demo.gif)

## The demo: a change the dashboard likes

[`demo/`](demo) is a small shop: a frontend, an orders service on Postgres
and a pricing service, instrumented with the official OpenTelemetry Go SDK
(`otelhttp` for HTTP, spans with the database conventions around each
query) and exporting to a real OpenTelemetry Collector, whose file exporter
writes the traces trace-diff reads.

The pull request under review makes two reasonable-looking changes:

- **orders loads items lazily**, the way an ORM association does: the item
  ids first, then each item when it is touched.
- **the frontend retries pricing**: pricing is sometimes slow, so the call
  now times out after 40 ms and tries again, up to three times.

Both pass the shop's own tests ([`shop_test.go`](demo/cmd/shop/shop_test.go):
lazy and eager loading return identical orders, the page is the same with or
without retries, a slow first quote is recovered). This is what a load test
showed for the three runs in `examples/shop/`, 300 requests each:

| run | errors | pages without a quote | p50 | p95 | p99 |
| --- | --- | --- | --- | --- | --- |
| before | 0 | 0 | 9.8 ms | 73.5 ms | 86.3 ms |
| the same code again | 0 | 0 | 10.0 ms | 68.6 ms | 83.6 ms |
| after the change | 0 | 0 | 9.6 ms | **50.4 ms** | **64.7 ms** |

By the dashboard, the change is an improvement. The traces say otherwise:
the spans per request nearly doubled (2,128 → 4,203), the order page now
runs up to 50 queries where it ran one, and one request in ten now calls
pricing two or three times. The p95 improved because the frontend stops
waiting for pricing's slow path, not because the slow path went away: the
pricing server still finishes the abandoned request, so on those requests it
does the slow work and then the fast work. That load moved somewhere the
frontend's latency graph cannot see.

Why the N+1 barely moves latency here: the queries go to a Postgres on the
same machine, about 0.03 ms each, so 50 of them cost 1.4 ms. With the
database 1 ms away, as a managed database usually is, they would cost 50.

## Quickstart

With Go 1.24 or newer:

```bash
git clone https://github.com/ahmed-hashim-pro/trace-diff && cd trace-diff
go build -o trace-diff ./cmd/trace-diff

./trace-diff check examples/shop/before.jsonl.gz examples/shop/rerun.jsonl.gz   # no change, exit 0
./trace-diff check examples/shop/before.jsonl.gz examples/shop/after.jsonl.gz   # the report above, exit 1
./trace-diff check examples/shop/before.jsonl.gz examples/shop/after.jsonl.gz --svg n-plus-one.svg
./trace-diff summary examples/shop/after.jsonl.gz       # what each request calls, and how often
./trace-diff show examples/shop/after.jsonl.gz 08f7b4dbb263   # one trace as a tree
```

To run the shop yourself (Docker; under two minutes once the images are
pulled and built):

```bash
make demo          # three runs through a real Collector, then the two comparisons
```

`go test ./...` runs the unit tests in a few seconds. `make bench` runs every
synthetic scenario over 200 seeds and the broken comparisons (the tables
below; 15 to 20 minutes on four cores).

## Using it

trace-diff reads OTLP JSON: what the OpenTelemetry Collector's
[file exporter](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/exporter/fileexporter)
writes, or OTLP/HTTP JSON request bodies, one object or many per file,
gzipped or not. To capture traces, either add a file exporter to the
Collector you already run, as [`demo/otelcol.yaml`](demo/otelcol.yaml) does:

```yaml
exporters:
  file:
    path: /out/traces.jsonl
```

or point your services straight at trace-diff, which speaks OTLP/HTTP
(protobuf, which the SDKs send by default, or JSON):

```bash
trace-diff collect --listen 127.0.0.1:4318 --out before.jsonl &
OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318 ./run-your-tests-or-load
kill %1   # flushes and prints how many spans it wrote
```

Then compare. `check` exits 1 when something should block the merge, 0
otherwise, 2 on bad input; `diff` prints the same report and always exits 0.
`--json` writes the report as JSON, `--svg` draws the traces behind a
finding, `--fail-on warn` fails on warnings too. In CI, record the base
branch and the pull request under the same load and compare:

```yaml
- run: ./trace-diff check base.jsonl pr.jsonl
```

The comparison is only as good as the load: both runs should send the same
kind of requests. trace-diff compares each endpoint on its own, so the mix
can differ, but an endpoint that only one run exercised is listed as a note,
not compared.

## What it reports

| kind | severity | what changed |
| --- | --- | --- |
| `n-plus-one` | fail | a call now repeated one after another under the same parent, per request |
| `fan-out` | fail | a call now repeated in parallel, more copies than before |
| `retry-storm` | fail | more requests where a call is repeated after a failed attempt |
| `new-sync-call` | fail | a new call that the request waits for (on its critical path) |
| `more-calls` | fail | more of an existing call per request |
| `latency` | fail | the endpoint got slower: the median, or the tail past the old p95 |
| `new-call` | warn | a new call the request does not wait for |
| `more-frequent` | warn | an existing call now made in more requests |
| `removed-call`, `fewer-calls`, `faster` | info | for context, or as what a new call replaced |

A new client call brings its server span and its queries with it; a retried
call repeats everything beneath it. Those are one change, so findings about a
subtree are folded into the finding about its top. A latency finding lists
where the time went: the operations whose time on the critical path grew most.

## How it decides

1. **Matching operations across runs.** Span ids differ between runs and so
   do the values in them, so a span is identified by what it did: service,
   kind, and a description with the values taken out. A database span is its
   statement with literals and bind parameters replaced (`WHERE id = 42` and
   `WHERE id = $1` both become `WHERE id = ?`, `IN (1, 2, 3)` becomes
   `IN (?)`); an HTTP server span is its route; an HTTP client span is the
   host and the path with ids replaced (`/orders/42` → `/orders/{n}`).
2. **Per endpoint, per request.** Traces are grouped by their root span
   (`frontend GET /orders/{id}`), and every comparison is between requests
   to the same endpoint: how many requests made the call, how many times
   each, how many of those followed a failed attempt, how many ran at once,
   and how long the request waited inside it.
3. **Is it real?** Each question is a statistical test: Fisher's exact test
   for "more requests do this", Mann-Whitney U for "requests do more of
   this" and for latency, Fisher again for the tail beyond the old p95. A
   comparison runs hundreds of tests, so they are corrected together
   (Benjamini-Hochberg, false discovery rate 0.001). Mann-Whitney matches
   scipy's to five decimal places ([`stats_test.go`](internal/stats/stats_test.go)).
4. **Is it big enough to matter?** A significant change is reported only if
   it clears a floor: ten points more of the requests, half a call more per
   request and 1.5 times as many, two points more of the requests retried,
   or 20% and 2 ms of latency. With enough traffic, a 2% shift is
   significant and still not worth blocking a merge.
5. **What shape is it?** A call that went up is named by how it went up:
   repeated under one parent one after another (`n-plus-one`) or at the same
   time (`fan-out`); repeated after a failed attempt (`retry-storm`); new and
   on the critical path (`new-sync-call`).

The critical path is the chain of work the request was actually waiting on:
walking back from the end of the root span, the child that finished last
before that point, then recursively the same inside it. Time a request spent
waiting inside a call, children included, is what "the request waits on it"
reports.

## Does it work?

[`internal/synth`](internal/synth/synth.go) generates a second, synthetic
shop with four endpoints, log-normal latencies, a cache that hits 70% of the
time, queries with literal ids inlined, an existing per-item call in a list
endpoint, retries on 3% of payments, and a traffic mix that moves by up to
±50% between the two runs. Each scenario plants one change in the after run,
or none. For each of 200 seeds, trace-diff compares 300 requests a side
(3,000 for drift), and a finding counts as found only with the right kind
about the right operation. "Unexpected" is anything else that was reported,
other than consequences of the planted change, like the latency it adds;
"failing" is the part of that which would fail `check`.

```
scenario        found     unexpected   failing   first unexpected finding
none            -         0/200        0/200
n-plus-one      200/200   0/200        0/200
new-sync-call   200/200   1/200        0/200     seed 9: INFO removed-call gateway GET /orders/{id}: called less: pricing · db · SELECT sku, price_cents FROM prices WHERE sku IN (?)
new-hop         200/200   0/200        0/200
retry-storm     200/200   0/200        0/200
fan-out         200/200   0/200        0/200
latency         200/200   0/200        0/200
drift           -         5/200        0/200     seed 101: WARN more-frequent gateway GET /orders/{id}: called in more requests: pricing · db · SELECT sku, price_cents FROM prices WHERE sku IN (?)
```

The one unexpected finding in `new-sync-call` and the five in `drift` are
the pricing cache miss rate, which moves by chance (seed 9) or by the six
planted points plus chance (drift), far enough to clear the ten-point floor.
None of them would fail `check`; that is what the warn and info levels are
for.

With 100 requests a side (1,000 for drift), the retry storm is found on 78
seeds of 200: it only affects the `GET /orders/{id}` requests, under half of
the traffic, and only the one call in four that fails, so there are not
enough retried requests to be sure. The other planted changes are still
found on all 200, and one drift seed produces a finding that would fail
`check`.

To show the tests above can fail, each of these deliberately broken
comparisons ([`analyze.Mutants`](internal/analyze/compare.go)) runs the
same scenarios:

```
mutant               result   what changed
keep-literals        caught   new-hop found 0/200, retry-storm found 0/200, fan-out found 0/200, and 1 more
pooled               caught   none unexpected on 137/200, n-plus-one found 150/200, n-plus-one unexpected on 117/200, and 11 more
no-correction        caught   n-plus-one unexpected on 1/200, new-sync-call unexpected on 2/200, new-hop unexpected on 1/200, and 2 more
no-floors            caught   latency unexpected on 1/200, drift unexpected on 76/200
any-repeat-is-retry  caught   n-plus-one found 0/200, n-plus-one unexpected on 200/200
```

Leaving out the correction for many tests shows only faintly, a few more
unexpected findings over 1,600 comparisons, because the size floors already
stop most of what it lets through. The floors are what keep drift quiet:
without them, 76 drift seeds of 200 report it.

## What this does not do

- **It does not run your load.** It compares what it is given. Two runs
  under different traffic, data or configuration will differ for those
  reasons, and trace-diff will report them. Run both under the same load
  test, or compare a canary against the baseline it shares traffic with.
- **It needs sampled traces to be comparable.** With head sampling, both
  runs should sample the same way. Tail sampling that keeps slow or failed
  traces skews both the counts and the latency.
- **It needs enough requests.** An endpoint with fewer than 20 requests on
  either side is skipped, and a change that affects one request in a hundred
  needs many hundreds to be seen.
- **It matches by what spans say.** Database spans need a statement
  (`db.query.text` or `db.statement`) to be told apart; without one they are
  matched by name. HTTP spans need a route or URL. A service that names
  spans with ids in them, beyond what the normalizer recognizes, makes every
  span look new.
- **It does not read OTLP protobuf files or gRPC.** Files are OTLP JSON; the
  receiver is OTLP/HTTP only.
- **Retries are recognized from the traces**: same-operation calls under one
  parent that follow a failed one, or that carry `http.request.resend_count`.
  A retry that happens inside a client library without its own span is
  invisible.
- **The demo is small**, one machine, 300 requests a run, and its changes
  were written to be caught. The synthetic benchmark measures trace-diff on
  changes it was designed for; it says nothing about kinds of regressions
  not listed above.

## Layout

```
cmd/trace-diff/      the CLI
internal/otlp/       OTLP JSON reading and writing, the OTLP/HTTP receiver
internal/analyze/    operation matching, per-trace features, the comparison
internal/stats/      Mann-Whitney U, Fisher's exact test, Benjamini-Hochberg
internal/report/     text, JSON and SVG output
internal/synth/      the synthetic shop and its scenarios
internal/bench/      the scenario benchmark and the mutants
demo/                the real shop: services, Collector config, compose file
examples/shop/       the three runs the README shows
docs/                the report, pictures and benchmark output (make docs)
```

## License

MIT
