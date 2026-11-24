package app

import (
	"fmt"
	"net/url"
)

// unparsableDatabaseURL is printed instead of any database URL that cannot be
// safely reduced to scheme://host/path. Raw URLs are never echoed: they
// routinely carry credentials in the userinfo or the query.
const unparsableDatabaseURL = "<unparsable database URL>"

// redactDatabaseURL renders a database URL without any credential material:
// only the scheme, host and path survive. Userinfo, query, fragment and opaque
// parts are dropped by construction (never url.URL.String, which re-emits
// them). Input that cannot be parsed into a scheme and a host maps to a fixed
// placeholder string.
func redactDatabaseURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return unparsableDatabaseURL
	}
	return fmt.Sprintf("%s://%s%s", parsed.Scheme, parsed.Host, parsed.Path)
}
