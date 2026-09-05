# Task 11 implementation report

## Outcome

Implemented the complete startup transaction in `run.go` and replaced the
reserved `runCommand` stub in `main.go`. The orchestrator now owns the lifetime
lock, transaction recovery, exact two-root mod candidate, held archive
descriptors, Steam backup/update boundary, known-good fallback, managed-file
rollback/promotion, log pruning, supervisor handoff, and typed process exits.

The obsolete Task 1 assertion that `run` printed `runtime not implemented` was
removed from `config_test.go`; its health, version, and usage dispatch coverage
remains. The SDD ledger was not edited.

## TDD evidence

Initial focused RED after creating `run_test.go`:

```text
$ go test -run TestRun ./...
./run_test.go:228:14: undefined: Application
./run_test.go:278:10: undefined: Application
./run_test.go:661:14: undefined: runError
FAIL github.com/saltydk/docker-vrising [build failed]
```

Initial focused GREEN after implementing `run.go` and `main.go`:

```text
$ go test -run TestRun ./...
ok github.com/saltydk/docker-vrising 0.006s
```

Additional RED/GREEN cycles covered missing first-install state, typed remote,
backup, and pre-mutation Steam failures without a fallback, canonical lock
handoff despite stale staging output, disabled-mod dependency validation, and
rollback after promotion failure. There are 25 `TestRun*` tests in the focused
suite.

## Verification evidence

```text
$ go test -race -run TestRun ./...
ok github.com/saltydk/docker-vrising 1.021s

$ go test -race -run TestRun -count=50 ./...
ok github.com/saltydk/docker-vrising 1.578s

$ go test -race ./...
ok github.com/saltydk/docker-vrising 59.338s

$ make check
go test ./...
ok github.com/saltydk/docker-vrising 52.029s
go vet ./...
```

All commands exited with status 0. `git diff --check` also exited with status 0.

## Self-review

- Mount validation precedes lifetime-lock acquisition; recovery is the first
  state mutation while the lock remains held through launch and promotion.
- Both configured roots are resolved together. Every package is fetched before
  staging, and every returned `ValidatedArchive` is closed after the final
  staging attempt on success and failure paths.
- Steam metadata and mutation occur only after the complete candidate is
  staged. Current builds skip backup/update; changed valid installs are backed
  up before `app_update`; unknown or post-mutation update failures fail closed.
- Known-good fallback is limited to pre-mutation failures with a validated
  installed build and a canonical active two-root lock. Degraded state records
  the reason before launch.
- Previously failed lock digests are rejected before staging. Disabled mods do
  not resolve, fetch, apply, rollback, or promote managed data.
- The selected canonical lock is copied into `LaunchRequest`; promotion occurs
  only after readiness. Failed candidates roll back, including promotion
  failures, and first candidates are never relaunched after failure.
- Reserved orchestration exits and the real post-readiness server exit status
  are preserved through `runError` and `runCommand`.
- `newApplication` is called only after identity resolution, root ownership
  preparation, privilege drop, and post-drop writable verification; no Store or
  runtime module is constructed before that barrier.
