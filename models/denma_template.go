package models

import (
	"bytes"
	"errors"
	"html/template"
	txttpl "text/template"
	"text/template/parse"
)

// DenmaTplOutputMax is the most one render of a template may write.
const DenmaTplOutputMax = 16 << 20

// ErrDenmaTplOutput is returned when a template writes more than DenmaTplOutputMax.
var ErrDenmaTplOutput = errors.New("the template's output is over 16 MB")

// DenmaTplBuffer is a bytes.Buffer for rendering templates, which stops the
// render at DenmaTplOutputMax.
type DenmaTplBuffer struct {
	bytes.Buffer
}

// Write appends p, or fails if that would pass DenmaTplOutputMax.
func (b *DenmaTplBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > DenmaTplOutputMax {
		return 0, ErrDenmaTplOutput
	}
	return b.Buffer.Write(p)
}

// ErrDenmaTplLoop is returned for a template that calls itself.
var ErrDenmaTplLoop = errors.New("a template can't call itself")

// DenmaNoLoopHTML passes on Parse's or AddParseTree's result, failing if the
// templates call each other in a loop.
func DenmaNoLoopHTML(t *template.Template, err error) (*template.Template, error) {
	if err != nil {
		return t, err
	}
	trees := map[string]*parse.Tree{}
	for _, x := range t.Templates() {
		trees[x.Name()] = x.Tree
	}
	return t, denmaTplLoops(trees)
}

// DenmaNoLoopText is DenmaNoLoopHTML for text templates.
func DenmaNoLoopText(t *txttpl.Template, err error) (*txttpl.Template, error) {
	if err != nil {
		return t, err
	}
	trees := map[string]*parse.Tree{}
	for _, x := range t.Templates() {
		trees[x.Name()] = x.Tree
	}
	return t, denmaTplLoops(trees)
}

func denmaTplLoops(trees map[string]*parse.Tree) error {
	const (
		visiting = iota + 1
		done
	)
	state := map[string]int{}
	var visit func(name string) bool
	visit = func(name string) bool {
		switch state[name] {
		case visiting:
			return true
		case done:
			return false
		}
		state[name] = visiting
		if tr := trees[name]; tr != nil && tr.Root != nil {
			for _, called := range denmaTplCalls(tr.Root, nil) {
				if visit(called) {
					return true
				}
			}
		}
		state[name] = done
		return false
	}
	for name := range trees {
		if visit(name) {
			return ErrDenmaTplLoop
		}
	}
	return nil
}

// denmaTplCalls adds the templates that n calls to out.
func denmaTplCalls(n parse.Node, out []string) []string {
	branch := func(b parse.BranchNode) {
		if b.List != nil {
			out = denmaTplCalls(b.List, out)
		}
		if b.ElseList != nil {
			out = denmaTplCalls(b.ElseList, out)
		}
	}
	switch x := n.(type) {
	case *parse.ListNode:
		for _, c := range x.Nodes {
			out = denmaTplCalls(c, out)
		}
	case *parse.TemplateNode:
		out = append(out, x.Name)
	case *parse.IfNode:
		branch(x.BranchNode)
	case *parse.RangeNode:
		branch(x.BranchNode)
	case *parse.WithNode:
		branch(x.BranchNode)
	}
	return out
}
