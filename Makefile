.PHONY: build trace test race race-pkg vtdiff cross clean

LDFLAGS := -s -w

# Release binaries are static: netgo leaves no package with cgo files (only
# net's libc resolver had any, and shelld resolves no names), so neither ld.so
# nor libc load at startup. CGO_ENABLED=0 is set per recipe; `make race` needs
# cgo. cmd/shelld's TestNetgoGraphHasNoCgo keeps the graph cgo-free.
BUILDTAGS := netgo

build:
	CGO_ENABLED=0 go build -trimpath -tags $(BUILDTAGS) -ldflags="$(LDFLAGS)" -o bin/shelld ./cmd/shelld
	CGO_ENABLED=0 go build -trimpath -tags $(BUILDTAGS) -ldflags="$(LDFLAGS)" -o bin/shellctl ./cmd/shellctl

# Tracing build: bin/shelld-traced records when SHELLD_TRACE=file is set;
# bin/shelld-trace analyzes the output. See internal/tracing.
trace:
	CGO_ENABLED=0 go build -trimpath -tags $(BUILDTAGS),shelld_trace -o bin/shelld-traced ./cmd/shelld
	CGO_ENABLED=0 go build -trimpath -tags $(BUILDTAGS) -o bin/shelld-trace ./cmd/shelld-trace

test:
	go vet ./...
	go vet -tags shelld_trace ./...
	go test ./...
	go test -tags shelld_trace ./internal/tracing

# -timeout is Go's per-PACKAGE budget, not the run's. The root package holds the
# differential and cast suites, and the 10-minute default alarm fires there for
# reasons unrelated to the code: its panic names whichever test was running, which
# reads like that test hanging, when the goroutine is `runnable` and the budget the
# alarm reports is the package's.
#
# The value is large rather than tight because the measurements span a factor of
# 1.6 from load alone: 717s at 1ea69e6 and 698s at faa6bc5 on an idle box, 1161s
# for the same work pinned to two CPUs at nice 19 under competition. A busier
# machine can plausibly exceed 30m, and exceeding it produces exactly the
# misleading panic this comment exists to prevent. A timeout is a safety net, not
# a budget: the only cost of 60m is that a genuinely hung test takes longer to be
# reported.
#
# -count=1 is not an optimization to remove. `go test` caches, and a cached race
# result is not a run: a second `make race` here replayed 13 of 14 packages and
# finished in 5.14s. That does not look like a skipped gate, it looks like a fast
# pass, because the output is a wall of `ok` either way. A gate's whole job is to
# answer "is this branch clean", so it has to cost what it costs; use race-pkg
# below while iterating.
#
# This recipe stays a single ./... on purpose — spelling out the other packages
# here means a package added later is silently left untested under -race.
race:
	go test -race -count=1 -timeout 60m ./...

# Partial race run for iterating, e.g. `make race-pkg PKG=./vt/`. Deliberately
# without -count=1: caching is what makes this useful, and this recipe makes no
# claim about the branch. `make race` is the gate.
race-pkg:
	go test -race -timeout 60m $(PKG)

# Differential test against shelld-rs's VtGrid (requires cargo and
# SHELLD_RS_DIR=/path/to/shelld-rs).
vtdiff:
	SHELLD_VTDIFF=1 go test . -run TestVtDiff -v

cross:
	GOOS=darwin GOARCH=arm64 go build -tags $(BUILDTAGS) -o /dev/null ./cmd/...
	GOOS=windows GOARCH=amd64 go vet ./...
	GOOS=windows GOARCH=amd64 go build -tags $(BUILDTAGS) -o /dev/null ./cmd/...

clean:
	rm -rf bin
