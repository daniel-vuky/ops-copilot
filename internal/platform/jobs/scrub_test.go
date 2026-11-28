package jobs

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestScrubURLs(t *testing.T) {
	t.Parallel()
	const secretURL = "https://user:pw@chat.example.test/v1/spaces/x/messages?key=k&token=t" //nolint:gosec // test fixture, not a credential
	tests := []struct {
		name, in, want string
	}{
		{
			name: "quoted http error",
			in:   `Post "` + secretURL + `": dial tcp: timeout`,
			want: `Post "https://chat.example.test": dial tcp: timeout`,
		},
		{ //nolint:gosec // test fixture, not a credential
			name: "two urls",
			in:   "a http://u:p@one.example.test:8080/p?q=1 and (https://two.example.test/x#frag).",
			want: "a http://one.example.test:8080 and (https://two.example.test).",
		},
		{name: "no url", in: "plain text: nothing here", want: "plain text: nothing here"},
		{name: "unparsable", in: "got http://[bad/x?key=k end", want: "got [url removed] end"},
		{name: "bad port", in: "x https://h:notaport/p?token=t", want: "x [url removed]"},
		{name: "no host", in: "open file:///etc/passwd now", want: "open [url removed] now"},
		{name: "trailing punctuation kept", in: "<https://h.example.test/a>;", want: "<https://h.example.test>;"},
		// Documented behaviour: a URL ends at the first white space, so a
		// query split by a space keeps its tail.
		{name: "url ends at white space", in: "https://h/p?key=k x&token=t", want: "https://h x&token=t"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ScrubURLs(tt.in); got != tt.want {
				t.Errorf("ScrubURLs(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestScrubURLsHTTPError scrubs the text of a *url.Error, the shape Go's
// HTTP client returns, which quotes the full request URL.
func TestScrubURLsHTTPError(t *testing.T) {
	t.Parallel()
	err := &url.Error{ //nolint:gosec // test fixture, not a credential
		Op:  "Post",
		URL: "https://user:pw@chat.example.test/v1/spaces/x/messages?key=k&token=t",
		Err: errors.New("dial tcp: timeout"),
	}
	got := ScrubURLs(err.Error())
	if want := `Post "https://chat.example.test": dial tcp: timeout`; got != want {
		t.Errorf("ScrubURLs(%q) = %q, want %q", err.Error(), got, want)
	}
	for _, leak := range []string{"user", "pw", "/v1/", "key=", "token="} {
		if strings.Contains(got, leak) {
			t.Errorf("ScrubURLs(%q) = %q, still contains %q", err.Error(), got, leak)
		}
	}
}

func TestCutError(t *testing.T) {
	t.Parallel()
	if got := cutError(strings.Repeat("a", 2000)); len(got) != 1024 {
		t.Errorf("len(cutError(2000 ASCII)) = %d, want 1024", len(got))
	}
	short := "short error"
	if got := cutError(short); got != short {
		t.Errorf("cutError(%q) = %q, want unchanged", short, got)
	}
	runes := strings.Repeat("ế", 700) // 3 bytes each, 2100 bytes
	got := cutError(runes)
	if len(got) > 1024 || !utf8.ValidString(got) {
		t.Errorf("cutError(3-byte runes): len %d, valid %v; want <= 1024 and valid", len(got), utf8.ValidString(got))
	}
	if len(got) != 1023 {
		t.Errorf("cutError(3-byte runes): len %d, want 1023 (341 whole runes)", len(got))
	}
}

func TestCutBytesInvalidUTF8(t *testing.T) {
	t.Parallel()
	got := cutBytes(strings.Repeat("\x80", 2000), maxErrorBytes)
	if got == "" || !utf8.ValidString(got) || len(got) > maxErrorBytes {
		t.Errorf("cutBytes(2000 invalid bytes) = %q (len %d, valid %v), want non-empty, valid, <= %d",
			got, len(got), utf8.ValidString(got), maxErrorBytes)
	}
	mixed := strings.Repeat("a", 1023) + "\xff" + strings.Repeat("b", 10)
	if got := cutBytes(mixed, maxErrorBytes); !utf8.ValidString(got) || len(got) > maxErrorBytes {
		t.Errorf("cutBytes(mixed) len %d valid %v, want valid and <= %d", len(got), utf8.ValidString(got), maxErrorBytes)
	}
}
