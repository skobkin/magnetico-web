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
