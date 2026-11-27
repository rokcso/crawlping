package main

import (
	"math"
	"sync"
	"time"
)

// Guard defaults. IndexNow does not publish its limits, so these are
// deliberately conservative: the job is to keep one site's retry loop from
// burning the operator's egress IP, not to squeeze the last request out of the
// engine. Raise them only with a reason and never past what the engine
// tolerates from one IP.
const (
	// rateBurstTokens is how many submissions one host may send back to back
	// before it has to wait for a token.
	rateBurstTokens = 10
	// rateRefillInterval is how long one host waits for its next token: one
	// submission per second, sustained, per host.
	rateRefillInterval = time.Second
)

// rateBucket is a per-host token bucket. It is the frequency half of the guard:
// a token is bound to one host, so nobody can submit for a domain they do not
// own, but a leaked token or a caller looping on a retry can hammer the engine
// from the operator's own IP until it throttles that IP - which breaks the
// legitimate submissions too, and is the harm this tool was built to avoid.
//
// The bucket is keyed on site.Host, so one noisy site cannot starve another.
// The mutex covers the arithmetic on the tokens and nothing else: it is never
// held across an engine call, and refill is computed from elapsed time rather
// than from a ticker, so there is no sleeping while holding a lock either.
//
// A refusal here is reasonThrottled and never reasonRateLimited: the first is
// this instance protecting its own egress IP and nothing was sent, the second is
// the engine already penalising that IP. A caller has to be able to tell which
// limit it hit, so the two reasons stay separate even though both answer 429.
type rateBucket struct {
	capacity float64
	// refill is tokens per second.
	refill float64
	// now is the clock. It is a field so a test can advance time instead of
	// sleeping; production always uses time.Now, whose readings stay monotonic
	// when subtracted, so a wall-clock jump cannot mint tokens.
	now func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucketTokens
}

// bucketTokens is one host's balance. last is the time the balance was last
// brought up to date.
type bucketTokens struct {
	tokens float64
	last   time.Time
}

func newRateBucket(capacity int, refillInterval time.Duration) *rateBucket {
	return &rateBucket{
		capacity: float64(capacity),
		refill:   1 / refillInterval.Seconds(),
		now:      time.Now,
		buckets:  make(map[string]*bucketTokens, 4),
	}
}

// take spends one token for host. It returns zero when a token was available,
// and otherwise how long the caller has to wait for the next one: a full burst
// is allowed, then one submission per refill interval.
func (b *rateBucket) take(host string) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()

	bucket, known := b.buckets[host]
	if !known {
		// A host starts full, so a site that has been quiet all day is not
		// slower on its first submission than on its tenth.
		bucket = &bucketTokens{tokens: b.capacity, last: now}
		b.buckets[host] = bucket
	}

	// Elapsed time, not a ticker: a bucket that was last touched five minutes
	// ago refills to capacity at the next look, with no goroutine behind it.
	if elapsed := now.Sub(bucket.last); elapsed > 0 {
		bucket.tokens = math.Min(b.capacity, bucket.tokens+elapsed.Seconds()*b.refill)
		bucket.last = now
	}

	if bucket.tokens >= 1 {
		bucket.tokens--
		return 0
	}

	// Round up, so the reported wait is never shorter than the real one and a
	// caller that waits exactly this long is not refused again on arrival.
	missing := 1 - bucket.tokens
	return time.Duration(math.Ceil(missing / b.refill * float64(time.Second)))
}
