# FINDINGS

Active, actionable bugs, security issues, and correctness problems.
Completed work and deprecation decisions are omitted.

Use the **ID** when commenting or assigning work. Update **Status** in the
same change as the code or decision, and remove the item when it is complete.
IDs are never reused, so a gap in the numbering only means the item was
resolved and removed.

## Status

| Status         | Meaning                                                |
| -------------- | ------------------------------------------------------ |
| Open           | Not started                                            |
| In Progress    | Being fixed                                            |
| Needs Approval | Behavior or compatibility change that needs a decision |

Severity: **security** > **bug** > **race** > **correctness** > **performance** > **docs**.

## Index

| ID  | Package | Title | Severity | Status |
| --- | ------- | ----- | -------- | ------ |
| [14](#14) | metrics | `ReadMemStats` stops the world every `ProfileInterval` | performance | Needs Approval |
| [15](#15) | cloudwatch | `WithCleanup` drops the interval when `PutMetricData` fails | correctness | Needs Approval |
| [16](#16) | cloudwatch | `Run` stops for good on a credentials error matched by string | correctness | Needs Approval |

---

<a id="14"></a>

## 14. `ReadMemStats` stops the world every `ProfileInterval`

**Package** metrics · **Severity** performance · **Status** Needs Approval ·
`metrics.go:124` (`emitRuntimeStats`)

`emitRuntimeStats` calls `runtime.ReadMemStats`, which stops the world, once per
`ProfileInterval` — one second by default, so every process built on
`DefaultConfig` pays it. On a large heap the pause is measurable.

The `runtime/metrics` package exposes the same counters without stopping the
world, and the names this package emits can stay identical. Verified
equivalences on Go 1.27:

| Emitted gauge | `runtime/metrics` name |
| --- | --- |
| `runtime_alloc_bytes` | `/memory/classes/heap/objects:bytes` |
| `runtime_sys_bytes` | `/memory/classes/total:bytes` |
| `runtime_malloc_count` | `/gc/heap/allocs:objects` |
| `runtime_free_count` | `/gc/heap/frees:objects` |
| `runtime_heap_objects` | `/gc/heap/objects:objects` |
| `runtime_total_gc_runs` | `/gc/cycles/total:gc-cycles` |

`runtime_num_goroutines` keeps using `runtime.NumGoroutine`, which does not
stop the world either and is not the same quantity as
`/sched/goroutines:goroutines` (the latter counts more).

**Why this is not done yet** Two metrics have no exact replacement:

- `runtime_gc_pause_ns` is emitted once per GC cycle from
  `MemStats.PauseNs`, which holds the sum of both stop-the-world pauses of
  that cycle. `runtime/metrics` only offers the
  `/sched/pauses/total/gc:seconds` histogram, which records each pause
  separately: about twice as many samples, each roughly half the value, and
  bucketed rather than exact.
- `runtime_total_gc_pause_ns` would have to be summed from the same histogram,
  which overestimates by the bucket width (~9% in a local check).

Both are values dashboards and alerts are built on, so switching needs a
decision: accept the new shape under the existing names, emit the new shape
under new names, or keep `ReadMemStats` behind an opt-in. See
[ROADMAP #1](ROADMAP.md) for the wider migration.

**Workaround until then** Raise `Config.ProfileInterval`, or set
`EnableRuntimeMetrics: false` and collect runtime metrics with
`collectors.NewGoCollector()` from the Prometheus client, which is already
based on `runtime/metrics`.

---

<a id="15"></a>

## 15. `WithCleanup` drops the interval when `PutMetricData` fails

**Package** cloudwatch · **Severity** correctness · **Status** Needs Approval ·
`cloudwatch/cloudwatch.go` (`Sink.Data`, `Sink.Flush`)

With `Config.WithCleanup`, `Data` deletes the aggregated datums from the sink
before `Flush` has published them. A `PutMetricData` failure — throttling, a
network error, a batch rejected for one bad datum — therefore loses the whole
interval, and with more than `maxMetricsPerRequest` datums the batches after
the failed one are lost as well, since `Flush` returns at the first error.
The error is logged, but the data is gone.

Without `WithCleanup` nothing is lost, because the running totals are
republished on the next interval; that is also the configuration that double
counts ([ROADMAP #4](ROADMAP.md)).

**Why this is not done yet** The fix is a policy decision: re-queue the failed
batch for the next flush (bounded, so a long outage cannot grow memory
without limit), or accept the loss and document it. Re-queueing changes the
timestamps CloudWatch sees and interacts with `MetricsExpiry`.

**Workaround until then** Keep `WithCleanup` off when a missing interval is
worse than a cumulative shape, or wrap `Sink.Publisher` with a client that
retries.

---

<a id="16"></a>

## 16. `Run` stops for good on a credentials error matched by string

**Package** cloudwatch · **Severity** correctness · **Status** Needs Approval ·
`cloudwatch/cloudwatch.go` (`Sink.Run`)

`Run` returns, ending all publishing for the life of the process, when the
error text of a flush contains `expired` or `NoCredentialProviders`. Two
problems:

- The check is a substring match on the message of an SDK v2 error, not a
  typed inspection; `NoCredentialProviders` is the SDK v1 wording, and any
  unrelated message containing `expired` also stops the loop.
- Credentials that expired are routinely refreshed (instance roles, IRSA,
  SSO), so giving up permanently is the wrong reaction; the next interval
  might succeed.

**Why this is not done yet** Stopping on a hard credentials failure was
deliberate, to avoid logging the same error every interval forever. Replacing
it needs a decision on the retry policy: keep trying with a backoff and a
rate-limited log line, or stop but expose the state so the caller can restart.
Belongs with [ROADMAP #4](ROADMAP.md).

**Workaround until then** Supervise `Run` from the caller: restart it when it
returns before the context is cancelled.
