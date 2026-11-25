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

const (
	dirSources    = "sources"
	dirScopes     = "scopes"
	dirPrincipals = "principals"
	dirCode       = "code"
	dirAuthority  = "authority"
	dirLLM        = "llm"
)

var configDirs = []string{dirSources, dirScopes, dirPrincipals, dirCode, dirAuthority, dirLLM}

var singleFileNames = []string{"hearsay.yaml", "hearsay.yml"}

// Load reads and validates the configuration at path, which is either a
// directory laid out as the configuration directories or a single YAML file
// holding the same thing under one key each (docs/config.md). A directory
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

type loader struct {
	path string
	root string

	sources    []doc[sourceDoc]
	scopes     []doc[scopeDoc]
	principals []doc[principalDoc]
	code       []doc[codeDoc]
	authority  []doc[authorityDoc]
	llm        []doc[llmDoc]

	files []readFile
	probs problems
}

type doc[T any] struct {
	file string
	line int
	v    T
}

type readFile struct {
	rel  string
	body []byte
}

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
	if d.LLM != nil {
		l.llm = []doc[llmDoc]{{file: rel, line: keyLine(root, dirLLM), v: *d.LLM}}
	}
}

type singleFileDoc struct {
	Sources    []sourceDoc    `yaml:"sources"`
	Scopes     []scopeDoc     `yaml:"scopes"`
	Principals []principalDoc `yaml:"principals"`
	Code       []codeDoc      `yaml:"code"`
	Authority  []authorityDoc `yaml:"authority"`
	LLM        *llmDoc        `yaml:"llm"`
}

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
	l.llm = readDir[llmDoc](l, dirLLM, "model tier section")
}

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

func (l *loader) read(rel string) ([]byte, bool) {
	body, err := os.ReadFile(filepath.Join(l.root, rel))
	if err != nil {
		l.probs.add(rel, 0, "", "reading the file: %v", err)
		return nil, false
	}
	l.files = append(l.files, readFile{rel: rel, body: body})
	return body, true
}

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

func (l *loader) strict(rel string, body []byte, v any, noun string) bool {
	dec := yaml.NewDecoder(bytes.NewReader(body))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		l.yamlProblems(rel, err, noun)
		return false
	}
	return true
}

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

func keyLine(mapping *yaml.Node, key string) int {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i].Line
		}
	}
	return 0
}

func seqLines(mapping *yaml.Node, key string) []int {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != key {
			continue
		}
		return nodeLines(mapping.Content[i+1].Content)
	}
	return nil
}

func nodeLines(nodes []*yaml.Node) []int {
	lines := make([]int, len(nodes))
	for i, n := range nodes {
		lines[i] = n.Line
	}
	return lines
}

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

func isYAML(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".yaml" || ext == ".yml"
}
