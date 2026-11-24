package opstats

import (
	"math/bits"
	"time"
)

// HourOf returns the start of the UTC hour containing t.
func HourOf(t time.Time) time.Time { return t.UTC().Truncate(time.Hour) }

// DayOf returns the start of the UTC day containing t.
func DayOf(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// MonthOf returns the start of the UTC month containing t.
func MonthOf(t time.Time) time.Time {
	y, m, _ := t.UTC().Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
}

// MinuteBit is the coverage bit of the UTC minute containing t: bit i of a
// per-hour mask stands for minute i.
func MinuteBit(t time.Time) int64 { return 1 << uint(t.UTC().Minute()) }

// FullHourMask has the 60 minute bits set.
const FullHourMask int64 = 1<<60 - 1

// BitCount is the number of covered minutes in a mask.
func BitCount(mask int64) int { return bits.OnesCount64(uint64(mask)) }
