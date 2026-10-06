package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// retryAfterCap is the longest Retry-After the agent will honor.
	// Larger or farther-future values are clamped; garbage is ignored.
	retryAfterCap = 5 * time.Minute

	jobPostBackoffBase = 500 * time.Millisecond
	jobPostBackoffCap  = 30 * time.Second

	// pollBackoffCap bounds the jittered exponential used after a failed
	// GET /v1/jobs/next. Retry-After is capped separately at retryAfterCap.
	pollBackoffCap = 5 * time.Minute
)

// parseRetryAfter reads a Retry-After header value at time now.
// Delta-seconds and the three HTTP-date formats are accepted.
// The boolean is false when the header is missing or garbage.
// A date in the past is valid and yields 0 (retry is allowed now).
// Honored durations are capped at retryAfterCap.
func parseRetryAfter(header string, now time.Time) (time.Duration, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, false
	}
	if header[0] >= '0' && header[0] <= '9' {
		secs, err := strconv.ParseInt(header, 10, 64)
		if err != nil || secs < 0 {
			return 0, false
		}
		maxSecs := int64(retryAfterCap / time.Second)
		if secs > maxSecs {
			return retryAfterCap, true
		}
		return time.Duration(secs) * time.Second, true
	}
	when, err := http.ParseTime(header)
	if err != nil {
		return 0, false
	}
	d := when.Sub(now)
	if d < 0 {
		return 0, true
	}
	if d > retryAfterCap {
		return retryAfterCap, true
	}
	return d, true
}

// honoredRetryAfter is the capped Retry-After carried on err, or 0 when
// the error has none. Only 503 and 429 responses populate the field.
func honoredRetryAfter(err error) time.Duration {
	var httpErr *hubHTTPError
	if !errors.As(err, &httpErr) || !httpErr.HasRetryAfter {
		return 0
	}
	if httpErr.RetryAfter < 0 {
		return 0
	}
	return httpErr.RetryAfter
}

// hubBusyWait is the check-in and observation delay after a failed post.
// On 503 with a parsed Retry-After the wait is max(server, jittered interval)
// and busy is true so the caller logs the compact line instead of the generic
// failure. floor is the server's request and is never larger than wait; the
// observation loop uses it to keep a nudge from posting earlier than that.
func hubBusyWait(jittered time.Duration, err error) (wait, floor time.Duration, busy bool) {
	wait = jittered
	var httpErr *hubHTTPError
	if err == nil || !errors.As(err, &httpErr) {
		return wait, 0, false
	}
	if httpErr.StatusCode != http.StatusServiceUnavailable || !httpErr.HasRetryAfter {
		return wait, 0, false
	}
	floor = httpErr.RetryAfter
	if floor < 0 {
		floor = 0
	}
	if floor > wait {
		wait = floor
	}
	return wait, floor, true
}

// hubBusyRetryLine is the single line logged for a 503 that carried Retry-After.
// Seconds are rounded up so the line never claims a shorter wait than the one
// the loop actually takes.
func hubBusyRetryLine(wait time.Duration) string {
	if wait < 0 {
		wait = 0
	}
	secs := int64((wait + time.Second - 1) / time.Second)
	return fmt.Sprintf("hub busy, retrying in %ds", secs)
}

func logHubBusy(wait time.Duration) {
	log.Printf("%s", hubBusyRetryLine(wait))
}

// defaultJobPostBackoff is full jitter over the exponential ceiling:
// uniform in [0, min(30s, 500ms*2^(attempt-1))]. attempt is 1-based, matching
// the post loop's first sleep. The Retry-After floor is applied by post, not here,
// so tests can inject RetryBackoff without losing the server's minimum.
func defaultJobPostBackoff(attempt int) time.Duration {
	return fullJitter(jobPostBackoffCeiling(attempt))
}

func jobPostBackoffCeiling(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	ceiling := jobPostBackoffBase
	for n := 1; n < attempt; n++ {
		if ceiling >= jobPostBackoffCap || ceiling > jobPostBackoffCap/2 {
			return jobPostBackoffCap
		}
		ceiling *= 2
	}
	if ceiling > jobPostBackoffCap {
		return jobPostBackoffCap
	}
	return ceiling
}

// fullJitter returns a uniform duration in [0, ceiling], inclusive.
// A non-positive ceiling yields 0. A rand failure yields the ceiling so a
// retry still backs off instead of spinning.
func fullJitter(ceiling time.Duration) time.Duration {
	if ceiling <= 0 {
		return 0
	}
	span := int64(ceiling) + 1
	if span <= 0 {
		return ceiling
	}
	n, err := rand.Int(rand.Reader, big.NewInt(span))
	if err != nil {
		return ceiling
	}
	return time.Duration(n.Int64())
}

// pollBackoffCeiling is pollInterval * 2^(failures-1), capped at pollBackoffCap.
// failures is 1 on the first failed poll.
func pollBackoffCeiling(interval time.Duration, failures int) time.Duration {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if failures < 1 {
		failures = 1
	}
	ceiling := interval
	for n := 1; n < failures; n++ {
		if ceiling >= pollBackoffCap || ceiling > pollBackoffCap/2 {
			return pollBackoffCap
		}
		ceiling *= 2
	}
	if ceiling > pollBackoffCap {
		return pollBackoffCap
	}
	return ceiling
}

// jobPollShouldBackOff reports whether a failed GET /v1/jobs/next should use
// the exponential poll delay. 503, 429, other 5xx, and transport errors do.
// 401 and other 4xx keep the normal poll interval.
func jobPollShouldBackOff(err error) bool {
	if err == nil {
		return false
	}
	var httpErr *hubHTTPError
	if !errors.As(err, &httpErr) {
		return true
	}
	switch httpErr.StatusCode {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return true
	}
	return httpErr.StatusCode >= 500
}
