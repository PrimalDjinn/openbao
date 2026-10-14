package cloudflare_d1

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openbao/openbao/sdk/v2/physical"
	physical_utils "github.com/openbao/openbao/v2/internal/physical/utils"
	"github.com/stretchr/testify/require"
)

var fastPolicy = requestPolicy{
	timeout:    2 * time.Second,
	maxRetries: 2,
	waitMin:    time.Millisecond,
	waitMax:    5 * time.Millisecond,
}

// scriptedServer answers the first len(statuses) requests with those statuses
// (0 means delegate to next) and every later request with next.
func scriptedServer(t *testing.T, next http.Handler, statuses ...int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1))
		if n <= len(statuses) && statuses[n-1] != 0 {
			writeD1Error(w, statuses[n-1], http.StatusText(statuses[n-1]))
			return
		}
		next.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

func TestD1BackendOverHTTP(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(newFakeD1(t))
	t.Cleanup(server.Close)
	db := newTestDB(t, server, fastPolicy)

	backend, err := physical_utils.NewGenericSQLBackend(physical_utils.GenericSQLBackendConfig{
		Client: &D1Executor{db: db}, Dialect: D1Dialect{},
	})
	require.NoError(t, err)
	require.NoError(t, backend.CreateTables(context.Background()))
	ok, err := db.Ping()
	require.NoError(t, err)
	require.True(t, ok)

	physical.ExerciseBackend(t, backend)
	physical.ExerciseBackend_ListPrefix(t, backend)
}

func TestQueryRetriesTransientFailures(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		server, calls := scriptedServer(t, newFakeD1(t), status, status)
		_, err := newTestDB(t, server, fastPolicy).QueryContext(context.Background(), "SELECT 1")
		require.NoError(t, err, "status %d", status)
		require.EqualValues(t, 3, calls.Load(), "status %d", status)
	}
}

func TestQueryStopsAfterMaxRetries(t *testing.T) {
	t.Parallel()
	server, calls := scriptedServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeD1Error(w, http.StatusServiceUnavailable, "unavailable")
	}))
	_, err := newTestDB(t, server, fastPolicy).QueryContext(context.Background(), "SELECT 1")
	require.ErrorContains(t, err, "after 3 attempt(s)")
	// The SDK's own retries (10 by default) must not multiply ours.
	require.EqualValues(t, 3, calls.Load())

	policy := fastPolicy
	policy.maxRetries = 0
	server, calls = scriptedServer(t, newFakeD1(t), http.StatusServiceUnavailable)
	_, err = newTestDB(t, server, policy).QueryContext(context.Background(), "SELECT 1")
	require.Error(t, err)
	require.EqualValues(t, 1, calls.Load())
}

func TestQueryDoesNotRetryPermanentFailures(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict} {
		server, calls := scriptedServer(t, newFakeD1(t), status)
		_, err := newTestDB(t, server, fastPolicy).QueryContext(context.Background(), "SELECT 1")
		require.Error(t, err, "status %d", status)
		require.EqualValues(t, 1, calls.Load(), "status %d", status)
	}

	// A SQL error is reported by D1 and is not retried either.
	server, calls := scriptedServer(t, newFakeD1(t))
	_, err := newTestDB(t, server, fastPolicy).QueryContext(context.Background(), "SELECT * FROM missing_table")
	require.Error(t, err)
	require.EqualValues(t, 1, calls.Load())
}

func TestQueryAttemptDeadline(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	server, calls := scriptedServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release) }) // runs before server.Close
	policy := fastPolicy
	policy.timeout = 100 * time.Millisecond
	policy.maxRetries = 1

	start := time.Now()
	_, err := newTestDB(t, server, policy).QueryContext(context.Background(), "SELECT 1")
	elapsed := time.Since(start)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.EqualValues(t, 2, calls.Load())
	require.Less(t, elapsed, 2*time.Second, "a hung D1 call must fail within its attempt deadlines")

	// Ping uses the same bound.
	start = time.Now()
	ok, err := newTestDB(t, server, policy).Ping()
	require.False(t, ok)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), 2*time.Second)
}

func TestQueryRetryAfter(t *testing.T) {
	t.Parallel()
	limited := func(retryAfter string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", retryAfter)
			writeD1Error(w, http.StatusTooManyRequests, "rate limited")
		})
	}

	// A rate-limit window longer than retry_wait_max fails at once rather than
	// holding the storage call open for minutes.
	server, calls := scriptedServer(t, limited("300"))
	start := time.Now()
	_, err := newTestDB(t, server, fastPolicy).QueryContext(context.Background(), "SELECT 1")
	require.Error(t, err)
	require.EqualValues(t, 1, calls.Load())
	require.Less(t, time.Since(start), time.Second)

	// A short Retry-After is honored.
	fake := newFakeD1(t)
	var n atomic.Int32
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			limited("0").ServeHTTP(w, r)
			return
		}
		fake.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	_, err = newTestDB(t, server, fastPolicy).QueryContext(context.Background(), "SELECT 1")
	require.NoError(t, err)
	require.EqualValues(t, 2, n.Load())
}

func TestQueryHonorsCallerContext(t *testing.T) {
	t.Parallel()
	server, calls := scriptedServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeD1Error(w, http.StatusServiceUnavailable, "unavailable")
	}))
	policy := fastPolicy
	policy.waitMin, policy.waitMax = 10*time.Second, 10*time.Second

	// Cancellation during the backoff returns promptly.
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, err := newTestDB(t, server, policy).QueryContext(ctx, "SELECT 1")
	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, time.Since(start), 2*time.Second)
	require.EqualValues(t, 1, calls.Load())

	// A caller deadline shorter than the backoff stops retries immediately.
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start = time.Now()
	_, err = newTestDB(t, server, policy).QueryContext(ctx, "SELECT 1")
	require.Error(t, err)
	require.Less(t, time.Since(start), 500*time.Millisecond)
	require.EqualValues(t, 2, calls.Load())
}

func TestNextWait(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	policy := requestPolicy{timeout: time.Second, maxRetries: 3, waitMin: 100 * time.Millisecond, waitMax: 400 * time.Millisecond}

	for attempt, ceiling := range []time.Duration{100, 200, 400} {
		wait, ok := policy.nextWait(ctx, attempt, context.DeadlineExceeded)
		require.True(t, ok)
		require.GreaterOrEqual(t, wait, ceiling*time.Millisecond/2)
		require.LessOrEqual(t, wait, ceiling*time.Millisecond)
	}
	_, ok := policy.nextWait(ctx, 3, context.DeadlineExceeded)
	require.False(t, ok, "retries are bounded")
	_, ok = policy.nextWait(ctx, 0, errors.New("local failure"))
	require.False(t, ok, "unknown errors are not retried")

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, ok = policy.nextWait(canceled, 0, context.Canceled)
	require.False(t, ok)
}

func TestParseRequestPolicy(t *testing.T) {
	t.Parallel()
	policy, err := parseRequestPolicy(map[string]string{})
	require.NoError(t, err)
	require.Equal(t, defaultRequestPolicy, policy)

	policy, err = parseRequestPolicy(map[string]string{
		"request_timeout": "2s", "max_retries": "4", "retry_wait_min": "50ms", "retry_wait_max": "3",
	})
	require.NoError(t, err)
	require.Equal(t, requestPolicy{timeout: 2 * time.Second, maxRetries: 4, waitMin: 50 * time.Millisecond, waitMax: 3 * time.Second}, policy)

	for _, conf := range []map[string]string{
		{"request_timeout": "0s"},
		{"request_timeout": "soon"},
		{"max_retries": "-1"},
		{"max_retries": "11"},
		{"max_retries": "two"},
		{"retry_wait_min": "0s"},
		{"retry_wait_min": "2s", "retry_wait_max": "1s"},
		{"retry_wait_max": "2m"},
	} {
		_, err := parseRequestPolicy(conf)
		require.Error(t, err, "%v", conf)
	}
}

func TestConcurrentQueries(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(newFakeD1(t))
	t.Cleanup(server.Close)
	db := newTestDB(t, server, fastPolicy)

	errs := make(chan error, 32)
	for i := range cap(errs) {
		go func() {
			_, err := db.QueryContext(context.Background(), "SELECT ?, ?, ?", "text", i, float64(i)/2)
			errs <- err
		}()
	}
	for range cap(errs) {
		require.NoError(t, <-errs)
	}
}
