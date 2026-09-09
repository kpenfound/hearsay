package config

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// Problem is one thing wrong with a configuration, located precisely enough to
// fix without searching: the file it is in, the line the offending object
// starts on, and which field of it.
type Problem struct {
	// File is the path of the file, relative to the configuration root.
	File string
	// Line is the line the object starts on, or 0 where the position is not
	// known.
	Line int
	// Where names the object and the field within it, such as
	// `scope "engine": sources[1]`. It is empty for a problem with the file
	// as a whole.
	Where string
	// Msg says what is wrong, and where it is useful what to do instead.
	Msg string
}

// Error renders the problem the way a compiler would, so an editor and a person
// can both find it. A problem with the configuration as a whole rather than
// with a file is just its message.
func (p *Problem) Error() string {
	parts := make([]string, 0, 3)
	if p.File != "" {
		loc := p.File
		if p.Line > 0 {
			loc = fmt.Sprintf("%s:%d", p.File, p.Line)
		}
		parts = append(parts, loc)
	}
	if p.Where != "" {
		parts = append(parts, p.Where)
	}
	return strings.Join(append(parts, p.Msg), ": ")
}

// InvalidError is everything wrong with a configuration, not just the first
// thing: fixing one problem at a time is what makes configuration painful, and
// the point of `hearsay config validate` is to hand back the whole list.
//
// It unwraps to its problems, so errors.As finds a [*Problem].
type InvalidError struct {
	// Path is the configuration root, as it was given to [Load].
	Path string
	// Problems is every problem found, ordered by file and line.
	Problems []*Problem
}

// Error lists every problem, one per line.
func (e *InvalidError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s is not a valid configuration: %d problem", e.Path, len(e.Problems))
	if len(e.Problems) != 1 {
		b.WriteString("s")
	}
	for _, p := range e.Problems {
		b.WriteString("\n  ")
		b.WriteString(p.Error())
	}
	return b.String()
}

// Unwrap exposes the problems to errors.Is and errors.As.
func (e *InvalidError) Unwrap() []error {
	errs := make([]error, len(e.Problems))
	for i, p := range e.Problems {
		errs[i] = p
	}
	return errs
}

// problems collects problems as loading and validation go, so that one pass
// reports everything rather than stopping at the first mistake.
type problems struct {
	list []*Problem
}

// add records a problem. The message is formatted here rather than by the
// caller so that every call site reads as one sentence.
func (p *problems) add(file string, line int, where, format string, args ...any) {
	p.list = append(p.list, &Problem{File: file, Line: line, Where: where, Msg: fmt.Sprintf(format, args...)})
}

// addProblem records an already-built problem, from YAML decoding.
func (p *problems) addProblem(pr *Problem) { p.list = append(p.list, pr) }

// any reports whether anything has been recorded.
func (p *problems) any() bool { return len(p.list) > 0 }

// err returns the accumulated problems as an error, ordered by file and line so
// that the output is the same on every run and a test can pin it. Problems on
// the same line keep the order they were found in, which is the order the
// fields appear in.
func (p *problems) err(path string) error {
	if !p.any() {
		return nil
	}
	slices.SortStableFunc(p.list, func(a, b *Problem) int {
		if c := cmp.Compare(a.File, b.File); c != 0 {
			return c
		}
		return cmp.Compare(a.Line, b.Line)
	})
	return &InvalidError{Path: path, Problems: p.list}
}
