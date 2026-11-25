package stats

import (
	"fmt"
	"time"
)

var windowSeconds = [3]int{60, 900, 3600}

// computeRates sums the rolling rates of the fresh instances (D8). An instance
// counts only if it is up (heartbeat younger than 30 s), observed at least half
// of the window, and its window ends within 30 s of the newest instance's;
// others are listed with the reason. Per instance the rate is count ÷ covered
// seconds, so the sum is the rate of the deployment; a window with no
// compatible instance has no value (unavailable, not zero).
func computeRates(instances []Instance) Rates {
	var newest time.Duration = -1 // smallest age of a window end = newest window
	for _, in := range instances {
		if in.State != StateUp {
			continue
		}
		if newest < 0 || in.windowEndAge < newest {
			newest = in.windowEndAge
		}
	}
	var r Rates
	factor := [3]float64{1, 60, 3600}
	unit := [3]string{"per second", "per minute", "per hour"}
	for w := 0; w < 3; w++ {
		win := &r.Windows[w]
		win.Seconds, win.Unit = windowSeconds[w], unit[w]
		var sum [metricCount]float64
		for _, in := range instances {
			switch {
			case in.State != StateUp:
				win.Excluded = append(win.Excluded, Excluded{in.ID, "no recent heartbeat"})
			case in.covered[w]*2 < windowSeconds[w]:
				win.Excluded = append(win.Excluded, Excluded{in.ID, fmt.Sprintf("observed only %d s of the %d s window", in.covered[w], windowSeconds[w])})
			case in.windowEndAge-newest > RateWindowSkew:
				win.Excluded = append(win.Excluded, Excluded{in.ID, "its window ends too long before the others'"})
			default:
				win.Included++
				for m := 0; m < metricCount; m++ {
					sum[m] += float64(in.counts[w][m]) / float64(in.covered[w]) * factor[w]
				}
			}
		}
		if win.Included > 0 {
			for m := range sum {
				win.Values[m] = known(sum[m])
			}
		}
	}
	return r
}

// utilisation is quota used ÷ quota max, unavailable when either is.
func utilisation(used, max Measured[int64]) Measured[float64] {
	if !used.Valid || !max.Valid || max.Value <= 0 {
		return Measured[float64]{}
	}
	return known(float64(used.Value) / float64(max.Value))
}

// ratio is num ÷ den from two sums (never a mean of ratios): unavailable when
// the denominator is zero.
func ratio(num, den int64) *float64 {
	if den <= 0 {
		return nil
	}
	v := float64(num) / float64(den)
	return &v
}

func ptr(v int64) *float64 { f := float64(v); return &f }

// aged returns a copy of o as it stands now, for an answer fetched at
// fetchedAt: ages grow by the time that has passed and every state is
// classified again (a sample that has expired in the meantime no longer shows
// its numbers), the rolling rates are recomputed from the aged instances and a
// count of an hour that has since closed is no longer "this hour". The receiver
// is shared by every reader of the cache and is never modified.
func (o Overview) aged(fetchedAt, now time.Time) Overview {
	elapsed := now.Sub(fetchedAt)
	if elapsed < 0 {
		elapsed = 0
	}
	if o.Schema != StateUp {
		return o
	}
	out := o
	out.Instances = make([]Instance, len(o.Instances))
	for i, in := range o.Instances {
		in.Age += elapsed
		in.windowEndAge += elapsed
		in.State = ClassifyIngest(in.Age)
		out.Instances[i] = in
	}
	out.Rates = computeRates(out.Instances)
	if q := &out.Queue; q.State == StateUp || q.State == StateStale {
		q.Age += elapsed
		if q.State = ClassifyQueue(q.Age); q.State == StateUnavailable {
			*q = QueueView{State: StateUnavailable, Age: q.Age}
		}
	}
	if s := &out.Storage; s.State == StateUp || s.State == StateStale {
		s.Age += elapsed
		if s.State = ClassifyStorage(s.Age); s.State == StateUnavailable {
			*s = StorageView{State: StateUnavailable, Age: s.Age}
		}
	}
	if !o.ThisHourAt.IsZero() && !now.UTC().Truncate(time.Hour).Equal(o.ThisHourAt.UTC().Truncate(time.Hour)) {
		out.ThisHourAt, out.ThisHour = time.Time{}, [metricCount]int64{}
	}
	return out
}
