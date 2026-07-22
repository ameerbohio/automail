// Standalone full-system E2E driver (testing-plan Part 5 / Goal T8). Its own
// module with ZERO external dependencies -- it speaks the product's public HTTP
// contract and the same crypto wire format the browser uses, both with the Go
// standard library, so `make scan` has nothing new to audit and the cloud/printer
// modules stay untouched. Runs only under `-tags e2e` against a live stack
// (scripts/e2e/full.sh); excluded from every default `go test ./...`.
module automail/e2e

go 1.25.0

toolchain go1.25.12
