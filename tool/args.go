package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/ChristopherDavenport/agenttool"
)

// exactKeys refuses arguments that name one of fields in another case:
// a key that is not a field but equals one without regard to case, as
// encoding/json compares them (strings.EqualFold, so "Path" and "PATH",
// and "dax_ſtamp" with the long s, are "path" and "dax_stamp").
//
// A claim reads a field by its exact key, and a tool's arguments are
// decoded into a struct, which takes any case and the last such key: in
// {"path":"hello.txt","Path":"victim.txt"} the claim would see
// hello.txt and the tool write victim.txt. Every claim that reads a
// field of the arguments refuses such a key through this check, which
// the policy treats as a call it cannot read, and every dax-coding tool
// refuses it again when it runs (exactArgs), so the two sides agree
// without relying on each other. Arguments that are not an object are
// left to the decoder that reads them.
func exactKeys(args json.RawMessage, fields ...string) error {
	if len(bytes.TrimSpace(args)) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil {
		return nil
	}
	for k := range m {
		for _, f := range fields {
			if k != f && strings.EqualFold(k, f) {
				return fmt.Errorf("argument %q is %q in another case; write it once, as %q", k, f, f)
			}
		}
	}
	return nil
}

// exactArgs is t refusing, when it runs, arguments with a key that is
// one of the fields of Args in another case (exactKeys), before the
// arguments are decoded into Args.
func exactArgs[Args any](t agenttool.Tool) agenttool.Tool {
	fields := jsonFields(reflect.TypeFor[Args]())
	return agenttool.Wrap(t, func(ctx context.Context, c agenttool.Call) (agenttool.Result, error) {
		if err := exactKeys(c.Args, fields...); err != nil {
			return agenttool.Result{}, err
		}
		return t.Execute(ctx, c)
	})
}

// jsonFields are the keys encoding/json decodes into the struct type t.
func jsonFields(t reflect.Type) []string {
	var out []string
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch name {
		case "-":
			continue
		case "":
			name = f.Name
		}
		out = append(out, name)
	}
	return out
}
