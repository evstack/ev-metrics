package utils

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// InvalidEndpoint replaces endpoints that cannot be parsed, so that their content is never echoed.
const InvalidEndpoint = "invalid-endpoint"

// RedactEndpoint reduces an endpoint URL to scheme://host[:port], dropping userinfo, path, query
// and fragment, which commonly carry credentials. Input without a scheme is treated as host[:port].
func RedactEndpoint(raw string) string {
	if !strings.Contains(raw, "://") {
		// url.Parse reads "host:9090" as scheme "host", so strip by hand
		if i := strings.IndexAny(raw, "/?#"); i >= 0 {
			raw = raw[:i]
		}
		if i := strings.LastIndexByte(raw, '@'); i >= 0 {
			raw = raw[i+1:]
		}
		return raw
	}

	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return InvalidEndpoint
	}
	return u.Scheme + "://" + u.Host
}

// RedactEndpoints redacts each endpoint and keeps the results distinct: every repeat of an already
// seen redacted form gets "#<index>" appended, where index is its position in raws.
func RedactEndpoints(raws []string) []string {
	out := make([]string, len(raws))
	seen := make(map[string]bool, len(raws))
	for i, raw := range raws {
		r := RedactEndpoint(raw)
		if seen[r] {
			r += "#" + strconv.Itoa(i)
		}
		seen[r] = true
		out[i] = r
	}
	return out
}

type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// RedactError returns err with every occurrence of the raw endpoints, and of their userinfo and
// path/query parts, removed from the message. The original error stays reachable through
// errors.Is and errors.As.
func RedactError(err error, raws ...string) error {
	if err == nil {
		return nil
	}

	repl := map[string]string{}
	for _, raw := range raws {
		if raw == "" {
			continue
		}
		repl[raw] = RedactEndpoint(raw)
		u, perr := url.Parse(raw)
		if perr != nil {
			continue
		}
		if u.User != nil {
			repl[u.User.String()+"@"] = ""
		}
		if p := u.RequestURI(); u.Host != "" && len(p) > 1 {
			repl[p] = ""
		}
		if p := u.EscapedPath(); len(p) > 1 {
			repl[p] = ""
		}
	}

	// longest first: Replacer prefers earlier pairs when several match at one position
	olds := make([]string, 0, len(repl))
	for old := range repl {
		olds = append(olds, old)
	}
	sort.Slice(olds, func(i, j int) bool {
		if len(olds[i]) != len(olds[j]) {
			return len(olds[i]) > len(olds[j])
		}
		return olds[i] < olds[j]
	})
	pairs := make([]string, 0, 2*len(olds))
	for _, old := range olds {
		pairs = append(pairs, old, repl[old])
	}

	return &redactedError{msg: strings.NewReplacer(pairs...).Replace(err.Error()), err: err}
}
