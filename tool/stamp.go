package tool

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// errChanged is what a call gets when it was allowed as a plan that
// the command no longer analyses to.
var errChanged = errors.New("the command changed since it was allowed (a file or the repository's git configuration is different now); ask again")

// StampArgs is the policy's side of an auto-allowed bash call: when the
// command line analyses as Auto, the arguments come back carrying the
// stamp of the plan that was approved, and the bash tool will run only
// that plan. Any other call comes back with a stamp the model supplied,
// which is not valid, removed. changed says the arguments differ.
func StampArgs(ctx context.Context, an *Analyzer, args json.RawMessage) (out json.RawMessage, changed bool, err error) {
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
	_, had := m["dax_stamp"]
	delete(m, "dax_stamp")
	if c := an.Check(ctx, cmd); c.Auto {
		s, _ := json.Marshal(stampOf(c.Render()))
		m["dax_stamp"] = s
		changed = true
	} else {
		changed = had
	}
	if !changed {
		return args, false, nil
	}
	out, err = json.Marshal(m)
	return out, true, err
}
