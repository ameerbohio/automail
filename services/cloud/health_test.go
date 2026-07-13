package main

// Observability gate for the readiness endpoint (Testing Goal T11 / Part 9):
// GET /healthz must report 503 when a backing store is unreachable and 200 only
// when both Postgres and Redis answer. Uses the fake SQL driver (dbfake_test.go)
// and miniredis; closing either simulates an outage.

import (
	"database/sql"
	"database/sql/driver"
	"net/http"
	"net/http/httptest"
	"testing"

	"automail/cloud/db"
	"automail/cloud/handlers"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newHealthServer(sqlDB *sql.DB, rdb *redis.Client) *handlers.Server {
	return &handlers.Server{Queries: db.New(sqlDB), SQLDB: sqlDB, Redis: rdb}
}

func nopQuery(string, []driver.NamedValue) ([]string, [][]driver.Value, error) {
	return nil, nil, nil
}

func TestHealthz_Readiness(t *testing.T) {
	code := func(srv *handlers.Server) int {
		w := httptest.NewRecorder()
		srv.Healthz(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		return w.Code
	}

	t.Run("healthy -> 200", func(t *testing.T) {
		mr := miniredis.RunT(t)
		sqlDB := sql.OpenDB(fakeConnector{q: nopQuery})
		defer sqlDB.Close()
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		if got := code(newHealthServer(sqlDB, rdb)); got != http.StatusOK {
			t.Fatalf("healthy readiness = %d, want 200", got)
		}
	})

	t.Run("postgres down -> 503", func(t *testing.T) {
		mr := miniredis.RunT(t)
		sqlDB := sql.OpenDB(fakeConnector{q: nopQuery})
		sqlDB.Close() // simulate the DB going away
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		if got := code(newHealthServer(sqlDB, rdb)); got != http.StatusServiceUnavailable {
			t.Fatalf("postgres-down readiness = %d, want 503", got)
		}
	})

	t.Run("redis down -> 503", func(t *testing.T) {
		mr := miniredis.RunT(t)
		sqlDB := sql.OpenDB(fakeConnector{q: nopQuery})
		defer sqlDB.Close()
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		mr.Close() // simulate Redis going away
		if got := code(newHealthServer(sqlDB, rdb)); got != http.StatusServiceUnavailable {
			t.Fatalf("redis-down readiness = %d, want 503", got)
		}
	})
}
