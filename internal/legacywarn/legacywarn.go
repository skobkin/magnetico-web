// Package legacywarn is a TEMPORARY compatibility aid, planned for removal
// around version 0.10.0.
//
// Magnetico Web 0.8.0 is a rewrite in Go. An operator who starts the new image
// or binary with the environment of the historical PHP deployment gets
// configuration errors that say nothing about the real cause. This package
// recognises that situation from the *names* of two environment variables that
// only the PHP application used and prints one prominent explanation. It never
// reads, logs or forwards a value, never translates configuration and never
// touches a database.
package legacywarn

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// UpgradeURL is where the upgrade instructions live.
const UpgradeURL = "https://github.com/skobkin/magnetico-web/blob/master/UPGRADE.md"

// triggers are the variables that identify the historical PHP deployment (see
// the .env of the master branch). Generic Symfony names such as APP_ENV,
// APP_SECRET, MAILER_DSN or REDIS_DSN are deliberately not triggers: other
// software uses them too.
var triggers = map[string]bool{
	"APP_DATABASE_URL":        true,
	"MAGNETICOD_DATABASE_URL": true,
}

// Detect returns the sorted, deduplicated names of the trigger variables that
// are present in environ ("NAME=value" entries; a variable exported with an
// empty value counts). Values are never part of the result.
func Detect(environ []string) []string {
	seen := map[string]bool{}
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if triggers[name] {
			seen[name] = true
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Warn writes the banner to w when environ contains a trigger variable and
// reports whether it did. Only variable names appear in the text.
func Warn(w io.Writer, environ []string) bool {
	names := Detect(environ)
	if len(names) == 0 {
		return false
	}
	const bar = "================================================================================"
	fmt.Fprintf(w, "%s\n", bar)
	fmt.Fprintf(w, "WARNING: the environment of the previous PHP deployment of Magnetico Web was detected\n")
	fmt.Fprintf(w, "         (variables set: %s).\n", strings.Join(names, ", "))
	fmt.Fprintf(w, "\n")
	fmt.Fprintf(w, "Magnetico Web has been rewritten in Go. The old deployment configuration and the old\n")
	fmt.Fprintf(w, "database layout are incompatible with this application. Explicit migration and\n")
	fmt.Fprintf(w, "configuration changes are required; nothing is migrated or translated automatically\n")
	fmt.Fprintf(w, "and no database is modified by this notice.\n")
	fmt.Fprintf(w, "\n")
	fmt.Fprintf(w, "Upgrade instructions: %s\n", UpgradeURL)
	fmt.Fprintf(w, "(Temporary notice; planned for removal around version 0.10.0.)\n")
	fmt.Fprintf(w, "%s\n", bar)
	return true
}
