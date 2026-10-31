package manager

import (
	"errors"
	"fmt"
	"html/template"
	"net/url"
	"reflect"
	"strings"
	txttpl "text/template"
)

// denmaTplValueMax is the longest text a template function takes or returns.
const denmaTplValueMax = 1 << 20

var errDenmaTplValue = errors.New("a template value is over 1 MB")

// denmaAllowedSprig names the sprig functions templates can use.
var denmaAllowedSprig = []string{
	// Conditions.
	"default", "empty", "coalesce", "ternary", "contains", "hasPrefix", "hasSuffix",
	// Text.
	"upper", "lower", "title", "trim", "trimAll", "trimPrefix", "trimSuffix", "trunc", "abbrev", "initials", "plural",
	// Dates.
	"date", "dateInZone", "htmlDate", "now", "toDate", "duration", "ago",
	// Numbers.
	"add", "add1", "sub", "mul", "div", "mod", "round", "ceil", "floor", "max", "min", "int", "int64", "float64", "atoi", "toString",
	// Attributes and lists.
	"get", "hasKey", "dig", "first", "last", "join",
}

// denmaTemplateFuncs returns the functions templates get from sprig: those
// allowed, and Go's own text functions, with a limit on the text each handles.
func denmaTemplateFuncs(sprigFuncs template.FuncMap) template.FuncMap {
	out := make(template.FuncMap, len(denmaAllowedSprig)+7)
	for _, name := range denmaAllowedSprig {
		if f, ok := sprigFuncs[name]; ok {
			out[name] = denmaLimitFunc(f)
		}
	}
	for name, f := range map[string]any{
		"print":    fmt.Sprint,
		"printf":   fmt.Sprintf,
		"println":  fmt.Sprintln,
		"html":     txttpl.HTMLEscaper,
		"js":       txttpl.JSEscaper,
		"urlquery": denmaURLQuery,
		"replace":  denmaReplace,
	} {
		out[name] = denmaLimitFunc(f)
	}
	return out
}

func denmaURLQuery(args ...any) string {
	return url.QueryEscape(fmt.Sprint(args...))
}

// denmaReplace is sprig's replace, refusing a result over denmaTplValueMax.
func denmaReplace(old, repl, src string) (string, error) {
	if n := strings.Count(src, old); len(src)+n*(len(repl)-len(old)) > denmaTplValueMax {
		return "", errDenmaTplValue
	}
	return strings.ReplaceAll(src, old, repl), nil
}

// denmaLimitFunc wraps f so that it fails on text over denmaTplValueMax, in
// its arguments or its result.
func denmaLimitFunc(f any) any {
	v := reflect.ValueOf(f)
	t := v.Type()

	in := make([]reflect.Type, t.NumIn())
	for i := range in {
		in[i] = t.In(i)
	}
	errType := reflect.TypeFor[error]()
	first := t.Out(0)
	typ := reflect.FuncOf(in, []reflect.Type{first, errType}, t.IsVariadic())

	return reflect.MakeFunc(typ, func(args []reflect.Value) []reflect.Value {
		fail := func(err error) []reflect.Value {
			return []reflect.Value{reflect.Zero(first), reflect.ValueOf(&err).Elem()}
		}
		for _, a := range args {
			if denmaTooLong(a) {
				return fail(errDenmaTplValue)
			}
		}
		var res []reflect.Value
		if t.IsVariadic() {
			res = v.CallSlice(args)
		} else {
			res = v.Call(args)
		}
		if len(res) == 2 && !res[1].IsNil() {
			return []reflect.Value{res[0], res[1]}
		}
		if denmaTooLong(res[0]) {
			return fail(errDenmaTplValue)
		}
		return []reflect.Value{res[0], reflect.Zero(errType)}
	}).Interface()
}

// denmaTooLong says whether v is text over denmaTplValueMax, or a list holding some.
func denmaTooLong(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Interface:
		return !v.IsNil() && denmaTooLong(v.Elem())
	case reflect.String:
		return v.Len() > denmaTplValueMax
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return v.Len() > denmaTplValueMax
		}
		for i := range v.Len() {
			if denmaTooLong(v.Index(i)) {
				return true
			}
		}
	}
	return false
}
