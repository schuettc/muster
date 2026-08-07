package remote

import "time"

// This file exists so the backoff can be tested as a pure function instead of
// by measuring elapsed time. A timing assertion on a jittered wait is a flaky
// test waiting to happen; pinning the jitter source and reading the duration
// back is exact.

// DefaultTimeout is the package's fallback per-attempt timeout, exposed so a
// test can assert the fallback lands on it without hardcoding the number.
const DefaultTimeout = defaultTimeout

// WaitFor exposes the unexported backoff computation for retry n (0-based).
func (c *Client) WaitFor(n int) time.Duration { return c.wait(n) }

// HTTPTimeout reports the per-attempt timeout New actually resolved.
func (c *Client) HTTPTimeout() time.Duration { return c.http.Timeout }

// WithJitterFrac pins the jitter source. f must return a value in [0,1).
func WithJitterFrac(f func() float64) Option {
	return func(c *Client) {
		if f != nil {
			c.jitter = f
		}
	}
}
