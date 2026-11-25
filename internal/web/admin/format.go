package admin

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/skobkin/magnetico-web/internal/web/admin/stats"
)

const dash = "—"

func groupDigits(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var out []byte
	for i := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

func approx(m bool, s string) string {
	if m {
		return "≈ " + s
	}
	return s
}

// fmtInt renders a count; an unavailable value is a dash, never 0.
func fmtInt(m stats.Measured[int64]) string {
	if !m.Valid {
		return dash
	}
	return approx(m.Approx, groupDigits(m.Value))
}

func bytesIEC(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 5; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func fmtBytes(m stats.Measured[int64]) string {
	if !m.Valid {
		return dash
	}
	return approx(m.Approx, bytesIEC(m.Value))
}

func fmtFloat(m stats.Measured[float64], unit string) string {
	if !m.Valid {
		return dash
	}
	s := fmt.Sprintf("%.4g", m.Value)
	if m.Value >= 100 {
		s = groupDigits(int64(m.Value + 0.5))
	}
	if unit != "" {
		s += " " + unit
	}
	return approx(m.Approx, s)
}

func fmtPercent(m stats.Measured[float64]) string {
	if !m.Valid {
		return dash
	}
	return fmt.Sprintf("%.1f %%", m.Value*100)
}

func fmtDuration(d time.Duration) string {
	switch {
	case d < 0:
		return "0 s"
	case d < 2*time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h %d min", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}

func fmtAge(m stats.Measured[time.Duration]) string {
	if !m.Valid {
		return dash
	}
	return approx(m.Approx, fmtDuration(m.Value))
}

// stateText describes a freshness state with its age.
func stateText(s stats.State, age time.Duration) string {
	switch s {
	case stats.StateUp:
		return "up"
	case stats.StateStale:
		return "stale (last seen " + fmtDuration(age) + " ago)"
	case stats.StateUninitialised:
		return "not initialised"
	}
	return "unavailable"
}

// stateClass is the CSS class of a state badge.
func stateClass(s stats.State) string {
	switch s {
	case stats.StateUp:
		return "ok"
	case stats.StateStale:
		return "warning"
	}
	return "error"
}

// tipped is a short rendering of a value with its exact form for the tooltip.
type tipped struct{ Text, Title string }

// compactCount renders a count with a decimal suffix and at most two decimals
// (1.48k, 76.54M); below 1,000 it is the plain number.
func compactCount(n int64) string {
	if n > -1000 && n < 1000 {
		return strconv.FormatInt(n, 10)
	}
	v, i := float64(n), -1
	for (v >= 999.995 || v <= -999.995) && i < 5 {
		v /= 1000
		i++
	}
	s := strings.TrimRight(strings.TrimRight(strconv.FormatFloat(v, 'f', 2, 64), "0"), ".")
	return s + string("kMGTPE"[i])
}

// tipCount is a compact count with the exact, digit-grouped value as its tooltip.
func tipCount(n int64) tipped { return tipped{Text: compactCount(n), Title: groupDigits(n)} }

// tipBytes is a size in IEC units with the exact byte count as its tooltip.
func tipBytes(n int64) tipped { return tipped{Text: bytesIEC(n), Title: groupDigits(n) + " bytes"} }

// relTime renders how long ago t was, seen from now ("just now", "5m ago",
// "7h ago", "20d ago"); the tooltip is the UTC time. A time in the future
// (a clock difference) is shown as the time itself.
func relTime(t, now time.Time) tipped {
	abs := t.UTC().Format("2006-01-02 15:04 UTC")
	d := now.Sub(t)
	var s string
	switch {
	case d < 0:
		s = abs
	case d < time.Minute:
		s = "just now"
	case d < time.Hour:
		s = fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		s = fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		s = fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return tipped{Text: s, Title: abs}
}

// fmtCount formats a registry count; a capped count is a lower bound ("10,000+").
func fmtCount(m stats.Measured[int64], capped bool) string {
	if !m.Valid {
		return dash
	}
	if capped {
		return groupDigits(m.Value) + "+"
	}
	return groupDigits(m.Value)
}

// authHitRate is the authentication cache efficiency of one window: hits over
// hits plus misses, both taken from the same window (a ratio of sums), or a
// dash when the window saw no credential.
func authHitRate(w stats.RateWindow) string {
	hits, miss := w.Values[stats.MetricIndex("auth_cache_hits")], w.Values[stats.MetricIndex("auth_cache_misses")]
	if !hits.Valid || !miss.Valid || hits.Value+miss.Value <= 0 {
		return dash
	}
	return fmt.Sprintf("%.0f %%", 100*hits.Value/(hits.Value+miss.Value))
}
