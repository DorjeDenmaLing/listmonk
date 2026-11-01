package models

import (
	"bytes"
	"errors"
	"html/template"
	"slices"
	txttpl "text/template"
	"text/template/parse"
	"time"
)

// DenmaTplOutputMax is the most one render of a template may write.
const DenmaTplOutputMax = 16 << 20

// ErrDenmaTplOutput is returned when a template writes more than DenmaTplOutputMax.
var ErrDenmaTplOutput = errors.New("the template's output is over 16 MB")

// DenmaTplTimeMax is the longest one render of a template may run.
const DenmaTplTimeMax = 5 * time.Second

// ErrDenmaTplTime is returned when a template runs longer than DenmaTplTimeMax.
var ErrDenmaTplTime = errors.New("the template took over 5 seconds")

// A render past denmaTplFreeTime runs for denmaTplRunTime in each denmaTplRunTime + denmaTplPauseTime.
const (
	denmaTplFreeTime  = 100 * time.Millisecond
	denmaTplRunTime   = 10 * time.Millisecond
	denmaTplPauseTime = 90 * time.Millisecond
)

// DenmaTplBuffer is a bytes.Buffer for rendering templates that slows and then limits a render.
type DenmaTplBuffer struct {
	bytes.Buffer
	start, paused time.Time
}

// Write appends p, or fails if the render is over either limit.
func (b *DenmaTplBuffer) Write(p []byte) (int, error) {
	now := time.Now()
	if b.start.IsZero() {
		b.start = now
	}
	took := now.Sub(b.start)
	if took > DenmaTplTimeMax {
		return 0, ErrDenmaTplTime
	}
	if took > denmaTplFreeTime && now.Sub(b.paused) > denmaTplRunTime {
		time.Sleep(denmaTplPauseTime)
		b.paused = time.Now()
	}
	if b.Len()+len(p) > DenmaTplOutputMax {
		return 0, ErrDenmaTplOutput
	}
	return b.Buffer.Write(p)
}

// Reset empties the buffer for another render, with its own time.
func (b *DenmaTplBuffer) Reset() {
	b.Buffer.Reset()
	b.start, b.paused = time.Time{}, time.Time{}
}

// ErrDenmaTplLoop is returned for a template that calls itself.
var ErrDenmaTplLoop = errors.New("a template can't call itself")

// DenmaTplGuardHTML passes on a parse result, failing on template loops and adding time checks.
func DenmaTplGuardHTML(t *template.Template, err error) (*template.Template, error) {
	if err != nil {
		return t, err
	}
	trees := map[string]*parse.Tree{}
	for _, x := range t.Templates() {
		trees[x.Name()] = x.Tree
	}
	return t, denmaTplGuard(trees)
}

// DenmaTplGuardText is DenmaTplGuardHTML for text templates.
func DenmaTplGuardText(t *txttpl.Template, err error) (*txttpl.Template, error) {
	if err != nil {
		return t, err
	}
	trees := map[string]*parse.Tree{}
	for _, x := range t.Templates() {
		trees[x.Name()] = x.Tree
	}
	return t, denmaTplGuard(trees)
}

func denmaTplGuard(trees map[string]*parse.Tree) error {
	if err := denmaTplLoops(trees); err != nil {
		return err
	}
	for _, tr := range trees {
		if tr != nil {
			denmaTplAddChecks(tr.Root)
		}
	}
	return nil
}

// denmaTplAddChecks starts each list with an empty text whose write checks the render's time.
func denmaTplAddChecks(l *parse.ListNode) {
	if l == nil {
		return
	}
	if len(l.Nodes) == 0 || !denmaTplIsCheck(l.Nodes[0]) {
		l.Nodes = slices.Insert(l.Nodes, 0, parse.Node(&parse.TextNode{NodeType: parse.NodeText, Text: []byte{}}))
	}
	branch := func(b parse.BranchNode) {
		denmaTplAddChecks(b.List)
		denmaTplAddChecks(b.ElseList)
	}
	for _, n := range l.Nodes {
		switch x := n.(type) {
		case *parse.IfNode:
			branch(x.BranchNode)
		case *parse.RangeNode:
			branch(x.BranchNode)
		case *parse.WithNode:
			branch(x.BranchNode)
		}
	}
}

func denmaTplIsCheck(n parse.Node) bool {
	x, ok := n.(*parse.TextNode)
	return ok && len(x.Text) == 0
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
