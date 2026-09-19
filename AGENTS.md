# RULES OF CONDUCT

This library provides a `metrics` package which can be used to instrument code,
plus the `prometheus`, `cloudwatch` and `factory` sink packages.
There is no service, agent loop, `cmd/`, or generated mock tree. Work in one package
at a time unless you are changing a documented internal dependency.

The root package must not import a sink subpackage: the `Sink` interface lives
in the root, and the import only goes one way.

## NAVIGATION

Do not start by grepping the tree.

1. Open [`Documentation/codemap.md`](Documentation/codemap.md). Use the
   concept index to find the owning file, then the package section for
   entry points and invariants.
2. Open that file (and its `_test.go`) before searching elsewhere.
3. Grep or glob only if the concept is missing from the map. When you find
   it, add it to the map in the same change.
4. For high-level purpose and install path, see [`README.md`](README.md).

## CODING GUIDELINES

### Style

- Do not use long one-liners for map or struct population; split
  key-value pairs on new lines for readability.
- Do not use many hardcoded strings or integers; define `const` at the
  top of the file or in the package.
- Memoize into variables; do not call functions with the same parameters
  more than once.

  Bad example:

```go
  if len(r.Tools) > 0 {
	  ts := make([]any, 0, len(r.Tools))
  }
```

### Errors

- Use `github.com/cockroachdb/errors` for all error creation and wrapping.
- Wrap external errors (filesystem, YAML/JSON, network, reflection, etc.)
  using either:
  - `errors.WithMessage(err, "unable to read file")` for static context.
  - `errors.Wrapf(err, "failed to resolve file %s", path)` when context
    includes dynamic values.
- Errors originated from internal sentinel types must also be wrapped so
  the stack is preserved. For `var ErrNotFound = errors.New("not found")`
  do not simply `return ErrNotFound`.
- Never ignore errors from serialization, filesystem, or network calls in
  library paths that already return `error`. Helpers that intentionally
  coerce and swallow errors (for example `values.FromJSON`, `print.JSON`)
  must keep that behavior documented in `Documentation/codemap.md`.
- Preserve sentinel errors used by callers (`errors.Is` / `errors.As`).
- Wrap with context that identifies the failed operation without losing
  the original cause.
- Keep error strings accurate after refactors. Do not leave stale package
  or function names in runtime errors.
- Panic reaches callers from `cloudwatch.dimensions` (more than 10 tags,
  via `logger.Panicf`) and from `metrics.NewInmemSink` / `InmemSink.Data`
  on an unusable interval or retention. None of these are an intended
  contract: they are tracked in `FINDINGS.md` (#3, #4, #8). Do not add new
  panic paths to a library call, and do not change an existing one without
  updating both the codemap and the finding.

### Tests

- Build tests using `assert` and `require` from
  `github.com/stretchr/testify`.
- Assert exact behavior, including key absence versus empty values and
  the real `err` from the call being tested.
- Keep test setup and cleanup trustworthy: fixtures should match their
  comments, and tests should actually execute in CI.
- Prefer table tests for conversion and formatting helpers.
- Put extra coverage in `*_extra_test.go` when extending an already large
  test file. Use `package foo_test` for black-box tests.
- Prefer the seams the packages already provide over sleeping:
  `prometheus.Sink.collectAtTime` takes the evaluation time, and
  `cloudwatch.Sink.Publisher` can be replaced with a fake client.
  `InmemSink` has no clock seam, so its tests do sleep across an interval
  boundary; keep those durations small and the intervals shorter.
- This module has no gomock-generated interfaces; do not introduce mocks
  unless the package under test cannot be exercised directly.

### Tools

- `make generate` : generate artifacts if interfaces require it (most
  packages here have none)
- `make fmt` : apply go fmt
- `make test` : test entire project
- `make lint` : final check
- `make all` : clean, tools, generate, coverage test

CI requires **92%** coverage (`MIN_TESTCOV` in `.github/workflows/unittest.yml`).
CI does not run `-race`; run `go test -race ./...` yourself before proposing
a change to shared state.

### Documentation

- Every package has a `doc.go` with a package comment and, where the
  package has a non-obvious entry point, a short usage example. All four
  packages (root, `prometheus`, `cloudwatch`, `factory`) have one.
- Document all exported types, functions, interfaces, and interface
  methods. Say what a symbol is _for_, not just what it is named.
- Keep samples in `doc.go`, the root `README.md`, and `Documentation/`
  accurate against the current API. A wrong sample is worse than no sample;
  compile a changed sample before committing it.
- When you add a package, add it to the sink table in the root `README.md`,
  add `doc.go`, and add a section plus concept-index rows in
  `Documentation/codemap.md`.

#### Keep `Documentation/codemap.md` current

Update the codemap in the **same change** when you add functionality or
make a major change, including:

- New, removed, or renamed package, subpackage, or file that owns a
  concept.
- New, moved, or renamed exported entry-point type, func, or interface.
- Changed invariants: panic vs error, process-global state, conversion
  zero-values, ID bit layout, interpolation syntax, goroutine/callback
  rules, or internal imports between packages.
- Changed test layout or testdata conventions.

The map must stay the navigation index: concept → file → entry points →
invariants. If you had to grep to find something that belongs there, add
the row.

## REPOSITORY MAP

Start here instead of grepping the tree.

- **[`Documentation/codemap.md`](Documentation/codemap.md)** — concept
  index, per-package files and entry points, invariants, internal
  dependencies, test layout.
- **[`README.md`](README.md)** — high-level overview, configuration
  reference and quick-start samples.
- **[`FINDINGS.md`](FINDINGS.md)** — known defects, referenced by ID from
  code comments. Read it before "fixing" surprising behavior: it may
  already be recorded, with the compatibility decision still open.
- **[`ROADMAP.md`](ROADMAP.md)** — larger planned work.
- Package `doc.go` in each of the four packages. There are no per-package
  `README.md` files.
