package jobs

import (
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// maxErrorBytes is the longest error text stored or logged (ADR 0006 §7).
const maxErrorBytes = 1024

// urlPattern matches anything shaped like a URL: a scheme, "://" and the
// rest up to white space.
var urlPattern = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://\S+`)

// urlTrailing are characters trimmed from the end of a URL match before it
// is parsed and put back after it, so the quote and colon of `Post
// "https://...": ...` are not taken for part of the URL.
const urlTrailing = `"'),.;:]>`

// urlRemoved replaces a URL-shaped substring that does not parse to a
// scheme and a host.
const urlRemoved = "[url removed]"

// ScrubURLs rewrites every URL-shaped substring of s, a scheme:// token up
// to the first white space, to scheme://host (the host keeps its port),
// dropping user info, path, query and fragment, since Go's HTTP errors
// quote the full request URL and a webhook URL carries its key and token
// (ADR 0006 §7). Text after a white space is not part of the URL, so a
// query split by a space keeps its tail. A substring that does not parse
// to a scheme and a host becomes "[url removed]". Text without a URL is
// returned unchanged. It is the default Options.Scrub.
func ScrubURLs(s string) string {
	return urlPattern.ReplaceAllStringFunc(s, scrubURL)
}

// scrubURL rewrites one URL match for ScrubURLs.
func scrubURL(m string) string {
	core := strings.TrimRight(m, urlTrailing)
	tail := m[len(core):]
	u, err := url.Parse(core)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return urlRemoved + tail
	}
	return u.Scheme + "://" + u.Host + tail
}

// cutError cuts s to at most maxErrorBytes bytes without splitting a
// UTF-8 sequence.
func cutError(s string) string {
	return cutBytes(s, maxErrorBytes)
}

// cutBytes cuts s to at most n bytes of valid UTF-8: invalid bytes become
// U+FFFD first, then the cut moves back to the start of a rune so a
// multi-byte sequence is never split.
func cutBytes(s string, n int) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	if len(s) <= n {
		return s
	}
	i := n
	for back := 0; back < utf8.UTFMax-1 && i > 0 && !utf8.RuneStart(s[i]); back++ {
		i--
	}
	return s[:i]
}
