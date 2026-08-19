package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
)

// Error is one configuration problem, located in the source file. A missing
// position (Line == 0) still yields a usable message thanks to Path.
type Error struct {
	File   string
	Line   int
	Column int
	Path   string // YAML path of the offending node, e.g. "checks[0].timeout"
	Msg    string
}

func (e Error) Error() string {
	var b strings.Builder
	b.WriteString(e.File)
	if e.Line > 0 {
		fmt.Fprintf(&b, ":%d", e.Line)
		if e.Column > 0 {
			fmt.Fprintf(&b, ":%d", e.Column)
		}
	}
	b.WriteString(": ")
	if e.Path != "" {
		b.WriteString(e.Path)
		b.WriteString(": ")
	}
	b.WriteString(e.Msg)
	return b.String()
}

// Errors is every problem found in one configuration, ordered by position.
// Validation never stops at the first problem: fixing a config one error per
// run is what makes people skip validation altogether.
type Errors []Error

func (es Errors) Error() string {
	switch len(es) {
	case 0:
		return "no configuration errors"
	case 1:
		return es[0].Error()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d configuration errors:", len(es))
	for _, e := range es {
		b.WriteString("\n  " + e.Error())
	}
	return b.String()
}

func (es Errors) sorted() Errors {
	sort.SliceStable(es, func(i, j int) bool {
		if es[i].Line != es[j].Line {
			return es[i].Line < es[j].Line
		}
		return es[i].Column < es[j].Column
	})
	return es
}

// source resolves YAML paths to positions in the original file.
type source struct {
	name string
	file *ast.File
}

// collector accumulates errors together with their source positions.
type collector struct {
	src  *source
	errs Errors
}

// addf records a problem at the node addressed by path (without the "$." root,
// e.g. "checks[0].timeout").
func (c *collector) addf(path, format string, args ...any) {
	line, col := c.src.pos(path)
	c.errs = append(c.errs, Error{
		File:   c.src.name,
		Line:   line,
		Column: col,
		Path:   path,
		Msg:    fmt.Sprintf(format, args...),
	})
}

// addKeyf records a problem at a specific key of the mapping addressed by path.
// Map keys (header names, body paths) can contain characters that the YAML path
// syntax cannot express, so they are located by scanning the mapping node.
func (c *collector) addKeyf(path, key, format string, args ...any) {
	line, col := c.src.mapKeyPos(path, key)
	if line == 0 {
		line, col = c.src.pos(path)
	}
	c.errs = append(c.errs, Error{
		File:   c.src.name,
		Line:   line,
		Column: col,
		Path:   fmt.Sprintf("%s[%s]", path, strconv.Quote(key)),
		Msg:    fmt.Sprintf(format, args...),
	})
}

func (c *collector) err() error {
	if len(c.errs) == 0 {
		return nil
	}
	return c.errs.sorted()
}

// pos resolves a path to a position, falling back to the closest enclosing node
// that does exist — an error about a missing field still points at the block it
// is missing from.
func (s *source) pos(path string) (line, col int) {
	for p := path; ; {
		if node := s.node(p); node != nil {
			if tk := node.GetToken(); tk != nil {
				return tk.Position.Line, tk.Position.Column
			}
		}
		if p == "" {
			return 0, 0
		}
		p = parentPath(p)
	}
}

func (s *source) mapKeyPos(path, key string) (line, col int) {
	mapping, ok := s.node(path).(*ast.MappingNode)
	if !ok {
		return 0, 0
	}
	for _, v := range mapping.Values {
		if v.Key != nil && v.Key.GetToken() != nil && v.Key.GetToken().Value == key {
			tk := v.Key.GetToken()
			return tk.Position.Line, tk.Position.Column
		}
	}
	return 0, 0
}

func (s *source) node(path string) ast.Node {
	if s == nil || s.file == nil {
		return nil
	}
	full := "$"
	if path != "" {
		if strings.HasPrefix(path, "[") {
			full += path
		} else {
			full += "." + path
		}
	}
	p, err := yaml.PathString(full)
	if err != nil {
		return nil
	}
	node, err := p.FilterFile(s.file)
	if err != nil {
		return nil
	}
	return node
}

// parentPath drops the last component of a YAML path.
func parentPath(path string) string {
	dot := strings.LastIndexByte(path, '.')
	br := strings.LastIndexByte(path, '[')
	cut := max(dot, br)
	if cut < 0 {
		return ""
	}
	return path[:cut]
}
