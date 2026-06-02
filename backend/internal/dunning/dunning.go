// Package dunning encodes the retry policy for failed charges.
//
// State transitions:
//
//	active   --charge ok--> active (NextAttemptAt = on-chain nextChargeAt)
//	active   --charge err-> past_due (NextAttemptAt = now + backoffs[0])
//	past_due --charge ok--> active
//	past_due --charge err-> past_due (NextAttemptAt = now + backoffs[attempts])
//	past_due --attempts > len(backoffs)-> cancelled (call deactivate())
package dunning

import "time"

type Policy struct{ Backoffs []time.Duration }

// NextRetry returns when the scheduler should try again, given how many
// attempts have failed in the current cycle (1-indexed: 1 == first failure).
// Returns ok=false when the policy has been exhausted; the caller should mark
// the sub cancelled and call deactivate() on-chain.
func (p Policy) NextRetry(attempts int, now time.Time) (time.Time, bool) {
	if attempts < 1 || attempts > len(p.Backoffs) {
		return time.Time{}, false
	}
	return now.Add(p.Backoffs[attempts-1]), true
}
