package main

import (
	"net/url"
	"strings"
)

// redirectTarget is where a website shortcut sends a request; rest is the
// escaped path after the name. A base with %s is a search, filled with what
// was typed after name/, so ys/lofi beats searches for "lofi beats".
// Otherwise path and query carry over, so gh/v3danth/hop opens that repo.
func redirectTarget(base, rest string, u *url.URL) string {
	if strings.Contains(base, "%s") {
		return fillSearch(base, searchTerm(rest, u))
	}
	if rest == "/" {
		rest = ""
	}
	return joinURL(base, rest, u.RawQuery)
}

// searchTerm rebuilds what the person typed after name/. The browser splits
// a term like "what is 2+2?" at the "?", so the query is part of the term.
func searchTerm(rest string, u *url.URL) string {
	raw := strings.TrimPrefix(rest, "/")
	if u.RawQuery != "" || u.ForceQuery {
		raw += "?" + u.RawQuery
	}
	term, err := url.PathUnescape(raw)
	if err != nil {
		return raw
	}
	return term
}

// fillSearch puts term where %s is, escaped for that spot: in a query a
// space becomes +, but in a path (en.wikipedia.org/wiki/%s) + is a literal
// plus, so a space must become %20 there.
func fillSearch(base, term string) string {
	escape := url.QueryEscape
	if i := strings.Index(base, "%s"); !strings.Contains(base[:i], "?") {
		escape = url.PathEscape
	}
	return strings.ReplaceAll(base, "%s", escape(term))
}

// joinURL appends an escaped path and a raw query to base, keeping any
// query or fragment base already has.
func joinURL(base, path, query string) string {
	base, fragment, hasFragment := strings.Cut(base, "#")
	base, baseQuery, _ := strings.Cut(base, "?")
	joined := strings.TrimRight(base, "/") + path
	if baseQuery != "" && query != "" {
		query = baseQuery + "&" + query
	} else if query == "" {
		query = baseQuery
	}
	joined = withQuery(joined, query)
	if hasFragment {
		joined += "#" + fragment
	}
	return joined
}

func withQuery(u, query string) string {
	if query == "" {
		return u
	}
	return u + "?" + query
}
