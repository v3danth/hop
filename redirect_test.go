package main

import (
	"net/url"
	"testing"
)

func TestRedirectTarget(t *testing.T) {
	const search = "https://s.test/?q=%s"
	tests := []struct {
		name, base, target, want string
	}{
		{"bare name", "https://a.test/", "/", "https://a.test"},
		{"path and query carry over", "https://a.test", "/x/y?z=1", "https://a.test/x/y?z=1"},
		{"base query is kept", "https://a.test/app?tab=1", "/x?z=2", "https://a.test/app/x?tab=1&z=2"},
		{"base query alone", "https://a.test/?tab=1", "/", "https://a.test?tab=1"},
		{"base fragment stays last", "https://a.test/#top", "/x?z=1", "https://a.test/x?z=1#top"},
		{"search", search, "/cats", "https://s.test/?q=cats"},
		{"search with space", search, "/lofi%20beats", "https://s.test/?q=lofi+beats"},
		{"search with percent", search, "/100%25", "https://s.test/?q=100%25"},
		{"question mark is part of the term", search, "/what%20is%202+2?really", "https://s.test/?q=what+is+2%2B2%3Freally"},
		{"trailing question mark", search, "/why?", "https://s.test/?q=why%3F"},
		{"search in a path uses %20", "https://en.wikipedia.org/wiki/%s", "/Go%20language", "https://en.wikipedia.org/wiki/Go%20language"},
		{"plus in a path search stays a plus", "https://en.wikipedia.org/wiki/%s", "/C++", "https://en.wikipedia.org/wiki/C++"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.target)
			if err != nil {
				t.Fatal(err)
			}
			if got := redirectTarget(tt.base, u.EscapedPath(), u); got != tt.want {
				t.Errorf("redirectTarget(%q, %q) = %q, want %q", tt.base, tt.target, got, tt.want)
			}
		})
	}
}
