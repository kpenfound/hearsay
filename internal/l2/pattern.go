package l2

import (
	"strings"
)

// MatchPath reports whether a slash-separated path matches a path pattern.
//
// The pattern language is the one `code/` configuration uses
// (docs/config.md#code): `*` is any run of characters within one path segment,
// `?` is one character that is not a slash, and `**` as a whole segment is any
// number of segments, including none. So `engine/server/**` matches
// `engine/server`, `engine/server/write.go` and `engine/server/a/b.go`, and
// `**/*.go` matches every Go file.
//
// A leading `./` or `/` on either side is ignored: both spell the repository
// root.
func MatchPath(pattern, path string) bool {
	pattern = strings.TrimPrefix(strings.TrimPrefix(pattern, "./"), "/")
	path = strings.TrimPrefix(strings.TrimPrefix(path, "./"), "/")
	path = strings.TrimSuffix(path, "/")
	if pattern == "" || path == "" {
		return false
	}
	return matchSegments(strings.Split(pattern, "/"), strings.Split(path, "/"))
}

func matchSegments(pattern, path []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			rest := pattern[1:]
			for i := 0; i <= len(path); i++ {
				if matchSegments(rest, path[i:]) {
					return true
				}
			}
			return false
		}
		if len(path) == 0 || !matchSegment(pattern[0], path[0]) {
			return false
		}
		pattern, path = pattern[1:], path[1:]
	}
	return len(path) == 0
}

// matchSegment is one segment against one glob segment.
func matchSegment(pattern, name string) bool {
	for len(pattern) > 0 {
		switch pattern[0] {
		case '*':
			rest := pattern[1:]
			for i := 0; i <= len(name); i++ {
				if matchSegment(rest, name[i:]) {
					return true
				}
			}
			return false
		case '?':
			if name == "" {
				return false
			}
		default:
			if name == "" || name[0] != pattern[0] {
				return false
			}
		}
		pattern, name = pattern[1:], name[1:]
	}
	return name == ""
}

// staticPrefix is the directory a pattern names before its first wildcard:
// `engine/server/**` is `engine/server`. It is what a CODEOWNERS rule is matched
// against, because ownership is of a directory and a pattern is a set of paths.
func staticPrefix(pattern string) string {
	pattern = strings.TrimPrefix(strings.TrimPrefix(pattern, "./"), "/")
	segments := strings.Split(pattern, "/")
	out := segments[:0:0]
	for _, s := range segments {
		if strings.ContainsAny(s, "*?[") {
			break
		}
		out = append(out, s)
	}
	return strings.Join(out, "/")
}
