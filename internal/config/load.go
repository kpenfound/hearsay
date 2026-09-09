package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// The five directories of the configuration repository
// (docs/design.md#configuration).
const (
	dirSources    = "sources"
	dirScopes     = "scopes"
	dirPrincipals = "principals"
	dirCode       = "code"
	dirAuthority  = "authority"
)

// configDirs is every directory the loader reads, in the order it reads them.
var configDirs = []string{dirSources, dirScopes, dirPrincipals, dirCode, dirAuthority}

// singleFileNames are the names a single-file configuration has when [Load] is
// pointed at the directory holding it rather than at the file itself.
var singleFileNames = []string{"hearsay.yaml", "hearsay.yml"}

// Load reads and validates the configuration at path, which is either a
// directory laid out as the five configuration directories or a single YAML
// file holding the same thing under one key each (docs/config.md). A directory
// containing a `hearsay.yaml` and no configuration directories is the second
// form.
//
// It reports every problem it finds rather than the first, as an
// [*InvalidError]. A Repo is returned only when there are none.
func Load(fsPath string) (Repo, error) {
	info, err := os.Stat(fsPath)
	if err != nil {
		return Repo{}, fmt.Errorf("reading configuration at %s: %w", fsPath, err)
	}

	l := &loader{path: fsPath}
	if info.IsDir() {
		single, err := singleFileIn(fsPath)
		if err != nil {
			return Repo{}, err
		}
		if single != "" {
			l.root = fsPath
			l.readSingleFile(single)
		} else {
			l.root = fsPath
			l.readDirs()
		}
	} else {
		l.root = filepath.Dir(fsPath)
		l.readSingleFile(filepath.Base(fsPath))
	}

	repo := l.build()
	if l.probs.any() {
		return Repo{}, l.probs.err(fsPath)
	}
	repo.Path = fsPath
	repo.Digest = l.digest()
	return repo, nil
}

// singleFileIn returns the name of the single configuration file in dir, or
// empty if dir is in the directory form. Two things are an error rather than a
// choice this makes on the reader's behalf: a directory holding both forms,
// because which of the two a source came from would decide what happens to it
// and nothing in the file says which; and a directory holding two candidates for
// the single file, because reading one of them and none of the other is how a
// whole file of configuration goes missing in silence.
func singleFileIn(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("reading configuration directory %s: %w", dir, err)
	}
	var singles, dirs []string
	for _, e := range entries {
		switch {
		case e.IsDir() && slices.Contains(configDirs, e.Name()):
			dirs = append(dirs, e.Name())
		case !e.IsDir() && slices.Contains(singleFileNames, e.Name()):
			singles = append(singles, e.Name())
		}
	}
	if len(singles) > 1 {
		return "", fmt.Errorf("%s holds %s: two single-file configurations, and only one of them would be read. Keep one",
			dir, strings.Join(singles, " and "))
	}
	if len(singles) == 1 && len(dirs) > 0 {
		return "", fmt.Errorf("%s holds both configuration forms: %s and the %s directory. Use one or the other",
			dir, singles[0], strings.Join(dirs, ", "))
	}
	if len(singles) == 0 {
		return "", nil
	}
	return singles[0], nil
}

// loader accumulates what one Load reads: the decoded objects with where each
// came from, the bytes for the digest, and every problem found on the way.
type loader struct {
	// path is what Load was given, for error messages.
	path string
	// root is the directory file paths in problems are relative to.
	root string

	sources    []doc[sourceDoc]
	scopes     []doc[scopeDoc]
	principals []doc[principalDoc]
	code       []doc[codeDoc]
	authority  []doc[authorityDoc]

	files []readFile
	probs problems
}

// doc is one decoded object and where it was written.
type doc[T any] struct {
	file string
	line int
	v    T
}

// readFile is one file the configuration was read from, kept for the digest.
type readFile struct {
	rel  string
	body []byte
}

// digest is sha256 over every file read, each contributing its path, its length
// and its bytes, in path order. Path and length are in there so that moving a
// line from one file to another, or splitting a file, changes the digest.
func (l *loader) digest() string {
	files := slices.Clone(l.files)
	slices.SortFunc(files, func(a, b readFile) int { return strings.Compare(a.rel, b.rel) })
	h := sha256.New()
	for _, f := range files {
		fmt.Fprintf(h, "%s\n%d\n", f.rel, len(f.body))
		h.Write(f.body)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// readSingleFile loads the single-file form: one mapping whose keys are the
// five directories, each holding what that directory would.
func (l *loader) readSingleFile(rel string) {
	body, ok := l.read(rel)
	if !ok {
		return
	}
	root, ok := l.parse(rel, body)
	if !ok {
		return
	}
	if root.Kind != yaml.MappingNode {
		l.probs.add(rel, root.Line, "", "want a mapping with %s keys, found %s", strings.Join(configDirs, ", "), nodeKind(root))
		return
	}

	var d singleFileDoc
	if !l.strict(rel, body, &d, "configuration") {
		return
	}
	l.sources = zip(rel, seqLines(root, dirSources), d.Sources)
	l.scopes = zip(rel, seqLines(root, dirScopes), d.Scopes)
	l.principals = zip(rel, seqLines(root, dirPrincipals), d.Principals)
	l.code = zip(rel, seqLines(root, dirCode), d.Code)
	l.authority = zip(rel, seqLines(root, dirAuthority), d.Authority)
}

// singleFileDoc is the single-file form: the five directories as five keys.
// Each holds a list, because a directory holds a list of things.
type singleFileDoc struct {
	Sources    []sourceDoc    `yaml:"sources"`
	Scopes     []scopeDoc     `yaml:"scopes"`
	Principals []principalDoc `yaml:"principals"`
	Code       []codeDoc      `yaml:"code"`
	Authority  []authorityDoc `yaml:"authority"`
}

// readDirs loads the directory form. Files that are not YAML are ignored — a
// README belongs in a configuration repository — but a directory that is not
// one of the five, or a subdirectory inside one of them, is a problem: silently
// reading none of it is how configuration goes missing.
func (l *loader) readDirs() {
	entries, err := os.ReadDir(l.root)
	if err != nil {
		l.probs.add(".", 0, "", "reading the configuration directory: %v", err)
		return
	}
	for _, e := range entries {
		name := e.Name()
		switch {
		case strings.HasPrefix(name, "."):
			// .git and friends.
		case e.IsDir() && !slices.Contains(configDirs, name):
			l.probs.add(name+"/", 0, "", "not a configuration directory: want one of %s", strings.Join(configDirs, ", "))
		case !e.IsDir() && isYAML(name):
			l.probs.add(name, 0, "", "a YAML file at the top level is not read: move it into one of %s, or point hearsay at it directly to use the single-file form",
				strings.Join(configDirs, ", "))
		}
	}

	l.sources = readDir[sourceDoc](l, dirSources, "source")
	l.scopes = readDir[scopeDoc](l, dirScopes, "scope")
	l.principals = readDir[principalDoc](l, dirPrincipals, "principal")
	l.code = readDir[codeDoc](l, dirCode, "code entity")
	l.authority = readDir[authorityDoc](l, dirAuthority, "authority policy")
}

// readDir decodes every YAML file in one configuration directory. A file holds
// one object or a list of them, so that a team can keep a file per source or
// one file for all of them.
func readDir[T any](l *loader, dir, noun string) []doc[T] {
	entries, err := os.ReadDir(filepath.Join(l.root, dir))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			l.probs.add(dir+"/", 0, "", "reading the directory: %v", err)
		}
		return nil
	}

	var out []doc[T]
	for _, e := range entries {
		name := e.Name()
		rel := path.Join(dir, name)
		switch {
		case strings.HasPrefix(name, "."):
		case e.IsDir():
			l.probs.add(rel+"/", 0, "", "subdirectories are not read: put the %s files directly in %s/", noun, dir)
		case isYAML(name):
			out = append(out, decodeFile[T](l, rel, noun)...)
		}
	}
	return out
}

// decodeFile decodes one file of a configuration directory.
func decodeFile[T any](l *loader, rel, noun string) []doc[T] {
	body, ok := l.read(rel)
	if !ok {
		return nil
	}
	root, ok := l.parse(rel, body)
	if !ok {
		return nil
	}

	switch root.Kind {
	case yaml.SequenceNode:
		var items []T
		if !l.strict(rel, body, &items, noun) {
			return nil
		}
		return zip(rel, nodeLines(root.Content), items)
	case yaml.MappingNode:
		var item T
		if !l.strict(rel, body, &item, noun) {
			return nil
		}
		return []doc[T]{{file: rel, line: root.Line, v: item}}
	default:
		l.probs.add(rel, root.Line, "", "want one %s or a list of them, found %s", noun, nodeKind(root))
		return nil
	}
}

// read reads a file and keeps its bytes for the digest.
func (l *loader) read(rel string) ([]byte, bool) {
	body, err := os.ReadFile(filepath.Join(l.root, rel))
	if err != nil {
		l.probs.add(rel, 0, "", "reading the file: %v", err)
		return nil, false
	}
	l.files = append(l.files, readFile{rel: rel, body: body})
	return body, true
}

// parse returns the root node of the file's single YAML document. An empty file
// and a file holding several documents are both problems: the first is almost
// always a half-finished edit, and the second is a form this loader does not
// read, so both would otherwise be configuration that silently does nothing.
func (l *loader) parse(rel string, body []byte) (*yaml.Node, bool) {
	dec := yaml.NewDecoder(bytes.NewReader(body))
	var root yaml.Node
	if err := dec.Decode(&root); err != nil {
		if errors.Is(err, io.EOF) {
			l.probs.add(rel, 0, "", "the file is empty")
			return nil, false
		}
		l.yamlProblems(rel, err, "")
		return nil, false
	}
	var more yaml.Node
	if err := dec.Decode(&more); err == nil {
		l.probs.add(rel, more.Line, "", "a second YAML document starts here: put the objects in one list instead")
		return nil, false
	}
	if len(root.Content) != 1 {
		l.probs.add(rel, root.Line, "", "the file is empty")
		return nil, false
	}
	return root.Content[0], true
}

// strict decodes the file into v, rejecting any field v does not have so that a
// misspelled key fails loudly instead of being ignored. It decodes the bytes
// again rather than the node parse returned, because a node decodes without the
// strictness and without the file's line numbers.
func (l *loader) strict(rel string, body []byte, v any, noun string) bool {
	dec := yaml.NewDecoder(bytes.NewReader(body))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		l.yamlProblems(rel, err, noun)
		return false
	}
	return true
}

// yamlProblems turns a YAML decoding failure into problems. yaml reports type
// and unknown-field errors as a [*yaml.TypeError] holding one string per
// mistake, each starting `line N: `, and reports a syntax error as a single
// error whose text starts `yaml: line N: `. There is nothing structured to read
// instead, so this parses those two shapes; the tests pin them, so a change in
// the library shows up as a failure rather than as a problem with no line
// number.
func (l *loader) yamlProblems(rel string, err error, noun string) {
	var typeErr *yaml.TypeError
	if errors.As(err, &typeErr) {
		for _, msg := range typeErr.Errors {
			line, text := splitYAMLLine(msg)
			l.probs.addProblem(&Problem{File: rel, Line: line, Msg: rewriteUnknownField(text, noun)})
		}
		return
	}
	msg := strings.TrimPrefix(err.Error(), "yaml: ")
	line, text := splitYAMLLine(msg)
	l.probs.addProblem(&Problem{File: rel, Line: line, Msg: text})
}

// splitYAMLLine takes the `line N: ` prefix off a yaml message and returns it as
// a line number. A message without one keeps its text and reports line 0.
func splitYAMLLine(msg string) (int, string) {
	rest, ok := strings.CutPrefix(msg, "line ")
	if !ok {
		return 0, msg
	}
	num, text, ok := strings.Cut(rest, ": ")
	if !ok {
		return 0, msg
	}
	n, err := strconv.Atoi(num)
	if err != nil {
		return 0, msg
	}
	return n, text
}

// rewriteUnknownField turns yaml's `field x not found in type config.sourceDoc`
// into something written in the vocabulary of the configuration format rather
// than of this package's Go types. An unexpected shape is left alone.
func rewriteUnknownField(text, noun string) string {
	name, ok := strings.CutPrefix(text, "field ")
	if !ok || noun == "" {
		return text
	}
	name, _, ok = strings.Cut(name, " not found in type ")
	if !ok {
		return text
	}
	return fmt.Sprintf("no such field %q in a %s", name, noun)
}

// zip pairs decoded objects with the lines they start on. The two come from two
// passes over the same bytes, so they are in the same order; a line is 0 if the
// passes ever disagree about how many objects there are, which loses a line
// number rather than a problem.
func zip[T any](rel string, lines []int, items []T) []doc[T] {
	out := make([]doc[T], len(items))
	for i, item := range items {
		line := 0
		if i < len(lines) {
			line = lines[i]
		}
		out[i] = doc[T]{file: rel, line: line, v: item}
	}
	return out
}

// seqLines is the line each item of the named key's list starts on.
func seqLines(mapping *yaml.Node, key string) []int {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != key {
			continue
		}
		return nodeLines(mapping.Content[i+1].Content)
	}
	return nil
}

// nodeLines is the line each node starts on.
func nodeLines(nodes []*yaml.Node) []int {
	lines := make([]int, len(nodes))
	for i, n := range nodes {
		lines[i] = n.Line
	}
	return lines
}

// nodeKind names a YAML node for an error message.
func nodeKind(n *yaml.Node) string {
	switch n.Kind {
	case yaml.MappingNode:
		return "a mapping"
	case yaml.SequenceNode:
		return "a list"
	case yaml.ScalarNode:
		return "a value"
	case yaml.AliasNode:
		return "an alias"
	default:
		return "something else"
	}
}

// isYAML reports whether a file in a configuration directory is one to read.
func isYAML(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".yaml" || ext == ".yml"
}
