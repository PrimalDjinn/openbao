package cloudflare_d1

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/cloudflare/cloudflare-go/v6"
	"github.com/hashicorp/go-secure-stdlib/parseutil"
)

// requestPolicy bounds every D1 REST call. Each attempt has its own deadline,
// and failed attempts are retried a bounded number of times, so a slow or
// failing D1 call returns an error instead of blocking storage indefinitely.
// All statements this backend issues are idempotent (upserts, keyed deletes,
// reads, CREATE ... IF NOT EXISTS), so retrying an attempt that may have been
// applied is safe.
type requestPolicy struct {
	timeout    time.Duration // deadline for one HTTP attempt
	maxRetries int           // retries after the first attempt
	waitMin    time.Duration // first backoff step
	waitMax    time.Duration // backoff ceiling and the longest Retry-After honored
}

var defaultRequestPolicy = requestPolicy{
	timeout:    5 * time.Second,
	maxRetries: 2,
	waitMin:    100 * time.Millisecond,
	waitMax:    time.Second,
}

const (
	maxConfiguredRetries = 10
	maxRetryWait         = time.Minute
)

func parseRequestPolicy(conf map[string]string) (requestPolicy, error) {
	policy := defaultRequestPolicy
	durations := []struct {
		key string
		dst *time.Duration
	}{
		{"request_timeout", &policy.timeout},
		{"retry_wait_min", &policy.waitMin},
		{"retry_wait_max", &policy.waitMax},
	}
	for _, d := range durations {
		raw, ok := conf[d.key]
		if !ok || raw == "" {
			continue
		}
		value, err := parseutil.ParseDurationSecond(raw)
		if err != nil {
			return requestPolicy{}, fmt.Errorf("invalid %s: %w", d.key, err)
		}
		*d.dst = value
	}
	if raw, ok := conf["max_retries"]; ok && raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil {
			return requestPolicy{}, fmt.Errorf("invalid max_retries: %w", err)
		}
		policy.maxRetries = value
	}

	switch {
	case policy.timeout <= 0:
		return requestPolicy{}, errors.New("request_timeout must be greater than zero")
	case policy.maxRetries < 0 || policy.maxRetries > maxConfiguredRetries:
		return requestPolicy{}, fmt.Errorf("max_retries must be between 0 and %d", maxConfiguredRetries)
	case policy.waitMin <= 0 || policy.waitMax <= 0:
		return requestPolicy{}, errors.New("retry_wait_min and retry_wait_max must be greater than zero")
	case policy.waitMin > policy.waitMax:
		return requestPolicy{}, errors.New("retry_wait_min must not exceed retry_wait_max")
	case policy.waitMax > maxRetryWait:
		return requestPolicy{}, fmt.Errorf("retry_wait_max must not exceed %s", maxRetryWait)
	}
	return policy, nil
}

// nextWait reports whether the failed attempt number attempt (zero-based) may
// be retried, and how long to wait first.
func (p requestPolicy) nextWait(ctx context.Context, attempt int, err error) (time.Duration, bool) {
	if attempt >= p.maxRetries || ctx.Err() != nil {
		return 0, false
	}
	wait := p.backoff(attempt)

	var apiErr *cloudflare.Error
	switch {
	case errors.As(err, &apiErr):
		if !retryableStatus(apiErr.StatusCode) {
			return 0, false
		}
		if after, ok := retryAfter(apiErr.Response); ok {
			// The API asked for a longer pause than we may hold a storage
			// call open (for example the account-wide API rate limit, which
			// blocks for minutes); fail now instead of stalling the caller.
			if after > p.waitMax {
				return 0, false
			}
			wait = max(wait, after)
		}
	case errors.Is(err, context.DeadlineExceeded):
		// Only this attempt's deadline expired; the caller's context is live.
	case isTransportError(err):
	default:
		return 0, false
	}

	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= wait {
		return 0, false
	}
	return wait, true
}

// backoff doubles from waitMin up to waitMax and draws uniformly from the
// upper half of that step, so concurrent callers do not retry in lockstep.
func (p requestPolicy) backoff(attempt int) time.Duration {
	step := min(p.waitMin<<attempt, p.waitMax)
	half := step / 2
	return half + rand.N(step-half+1)
}

func retryableStatus(code int) bool {
	return code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= http.StatusInternalServerError
}

func retryAfter(res *http.Response) (time.Duration, bool) {
	if res == nil {
		return 0, false
	}
	value := res.Header.Get("Retry-After")
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		return max(0, time.Duration(seconds)*time.Second), true
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(0, time.Until(at)), true
	}
	return 0, false
}

func isTransportError(err error) bool {
	var urlErr *url.Error
	var netErr net.Error
	return errors.As(err, &urlErr) || errors.As(err, &netErr) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// newHTTPClient returns a client whose HTTP/2 connections are health-checked:
// a connection that stops answering is closed after about 20 seconds instead
// of absorbing every later request until the attempt deadlines expire.
func newHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.HTTP2 = &http.HTTP2Config{
		SendPingTimeout: 15 * time.Second,
		PingTimeout:     5 * time.Second,
	}
	return &http.Client{Transport: transport}
}
