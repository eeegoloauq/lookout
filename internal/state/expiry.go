package state

import (
	"math"
	"time"
)

// Numbered expiry tiers. After the last one, notices are daily.
var (
	CertTiers   = []int{21, 14, 7, 3}
	DomainTiers = []int{60, 30, 14, 7}
)

// DomainStaleAfter is how long a registry may stay silent before we
// tell someone. A single failed lookup is not a domain outage.
const DomainStaleAfter = 72 * time.Hour

// DaysLeft is whole 24-hour periods from now until expires, floored.
// Negative means already expired. Zero is "less than a day".
func DaysLeft(expires, now time.Time) int {
	if expires.IsZero() {
		return 0
	}
	return int(math.Floor(expires.Sub(now).Hours() / 24))
}

func utcDate(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

// nextTier decides whether a notice is due. It fires the tightest
// numbered threshold the remaining days have entered, exactly once,
// then daily after the last numbered tier. fired/dailyOn are updated
// in place via the return values so the caller can persist them.
func nextTier(days int, tiers []int, fired uint32, dailyOn string, now time.Time) (threshold int, newFired uint32, newDaily string, fire bool) {
	newFired = fired
	newDaily = dailyOn
	if len(tiers) == 0 {
		return 0, newFired, newDaily, false
	}

	current := -1
	for i, t := range tiers {
		if days <= t {
			current = i
		}
	}
	today := utcDate(now)
	last := tiers[len(tiers)-1]

	if current >= 0 {
		bit := uint32(1) << uint(current)
		if newFired&bit == 0 {
			for i := 0; i <= current; i++ {
				newFired |= uint32(1) << uint(i)
			}
			if days < last {
				newDaily = today
			}
			return tiers[current], newFired, newDaily, true
		}
	}

	if days < last && newDaily != today {
		newDaily = today
		return 0, newFired, newDaily, true
	}
	return 0, newFired, newDaily, false
}

func renewed(prev, next time.Time) bool {
	return !prev.IsZero() && !next.IsZero() && next.After(prev)
}
