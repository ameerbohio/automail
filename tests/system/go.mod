// System-test drivers: the suites that exercise the ASSEMBLED product from
// outside, over its public HTTP contract, rather than any one service's
// internals. Six suites live here, one per build tag -- see README.md for the
// suite/tag/command table.
//
// Its own module with ZERO external dependencies -- it speaks the product's
// public HTTP contract and the same crypto wire format the browser uses, both
// with the Go standard library, so `make scan` has nothing new to audit and the
// cloud/printer modules stay untouched. Every suite is behind a build tag, so
// all of them are excluded from every default `go test ./...`.
module automail/tests/system

go 1.25.0

toolchain go1.25.13
