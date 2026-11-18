package daemoncore

import (
	"html"
	"regexp"
	"strings"
)

var htmlTagRe = regexp.MustCompile(`<[^>]*>`)

// cleanDescription turns a Spotify description, which may contain HTML links
// and entities (e.g. `Also try <a href="spotify:playlist:...">X</a>` or
// `&#x27;`), into plain text.
func cleanDescription(s string) string {
	if strings.ContainsAny(s, "<&") {
		s = html.UnescapeString(htmlTagRe.ReplaceAllString(s, ""))
	}
	return strings.TrimSpace(s)
}
