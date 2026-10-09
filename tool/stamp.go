package tool

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/ChristopherDavenport/agenttool"
)

// stampKey signs the plans the policy approves. It lives as long as the
// process, so a stamp cannot be carried to another session, and the
// model, which never sees it, cannot make one.
var stampKey = func() []byte {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		panic(err)
	}
	return k
}()

func stampOf(rendered string) string {
	mac := hmac.New(sha256.New, stampKey)
	mac.Write([]byte(rendered))
	return hex.EncodeToString(mac.Sum(nil))
}

// errTouched is what a file tool's call gets when what it touches is no
// longer what the policy decided on: a path that led to one file when
// the call was allowed leads to another now.
var errTouched = errors.New("what this call touches changed since it was allowed (a path leads somewhere else now); ask again")

// factsStamp signs the facts a call of the tool name was decided on,
// under the same key as bash's plans: the tool's name and each call it
// amounts to, in order, its tool and its arguments. A question's text is
// left out; it says nothing about what is touched. The "facts" prefix
// keeps it from ever equalling a plan's stamp.
func factsStamp(name string, calls []agenttool.FactCall) string {
	var b strings.Builder
	b.WriteString("facts\x00")
	b.WriteString(name)
	for _, c := range calls {
		b.WriteString("\x00")
		b.WriteString(c.Tool)
		b.WriteString("\x00")
		b.Write(c.Args)
	}
	return stampOf(b.String())
}

// withStamp is args with the dax_stamp field set to stamp, replacing
// any the model supplied.
func withStamp(args json.RawMessage, stamp string) (json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil {
		return nil, err
	}
	s, _ := json.Marshal(stamp)
	m["dax_stamp"] = s
	return json.Marshal(m)
}

// errChanged is what a call gets when it was allowed as a plan that
// the command no longer analyses to.
var errChanged = errors.New("the command changed since it was allowed (a file or the repository's git configuration is different now); ask again")

// StampArgs is the policy's side of an auto-allowed bash call: when the
// command line analyses as Auto, the arguments come back carrying the
// stamp of the plan that was approved, and the bash tool will run only
// that plan. Any other call comes back with a stamp the model supplied,
// which is not valid, removed. changed says the arguments differ.
// Arguments with a key that is command or dax_stamp in another case are
// an error (exactKeys).
func StampArgs(ctx context.Context, an *Analyzer, args json.RawMessage) (out json.RawMessage, changed bool, err error) {
	if err := exactKeys(args, "command", "dax_stamp"); err != nil {
		return nil, false, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil {
		return nil, false, err
	}
	var cmd string
	if raw, ok := m["command"]; ok {
		if err := json.Unmarshal(raw, &cmd); err != nil {
			return nil, false, err
		}
	}
	out, err = stampWith(an.Check(ctx, cmd), args)
	if err != nil || out == nil {
		return args, false, err
	}
	return out, true, nil
}

// stampWith is StampArgs given the analysis of the call's command: the
// arguments with the stamp of c's plan when c is Auto, or with a stamp
// the model supplied taken off; nil when they would not change.
func stampWith(c *Check, args json.RawMessage) (json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil {
		return nil, err
	}
	_, had := m["dax_stamp"]
	delete(m, "dax_stamp")
	if c.Auto {
		s, _ := json.Marshal(stampOf(c.Render()))
		m["dax_stamp"] = s
	} else if !had {
		return nil, nil
	}
	return json.Marshal(m)
}
