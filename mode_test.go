package dax

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/dax/internal/config"
	"github.com/ChristopherDavenport/dax/policy"
)

// /mode reads and switches the session's permission mode, in every
// front, and dax serve offers it to dax attach as a named command.
func TestTheModeCommand(t *testing.T) {
	r := startFront(t, &steps{}, nil)
	ctx := context.Background()
	res, err := command(ctx, r.sess, r.sess, "/mode", false)
	if err != nil || !strings.HasPrefix(res.out, "mode default\nmodes: default, acceptEdits, dontAsk, bypassPermissions") {
		t.Fatalf("/mode: %q, %v", res.out, err)
	}
	if res, err = command(ctx, r.sess, r.sess, "/mode dontask", false); err != nil || res.out != "[mode dontAsk]" {
		t.Fatalf("/mode dontask: %q, %v", res.out, err)
	}
	if _, err = command(ctx, r.sess, r.sess, "/mode plan", false); err == nil {
		t.Error("/mode plan was taken")
	}
	cmd := serveCommands(r.sess, r.sess, "")[cmdMode]
	for _, tc := range []struct{ args, want string }{
		{``, `{"mode":"dontAsk"}`},
		{`{"next":true}`, `{"mode":"default"}`},
		{`{"name":"acceptEdits"}`, `{"mode":"acceptEdits"}`},
	} {
		out, err := cmd.Run(ctx, json.RawMessage(tc.args))
		if err != nil || string(out) != tc.want {
			t.Errorf("served mode %s: %s, %v; want %s", tc.args, out, err, tc.want)
		}
	}
	if _, err := cmd.Run(ctx, json.RawMessage(`{"name":"default","next":true}`)); err == nil {
		t.Error("a name and next were both taken")
	}
}

// Shift-Tab moves the terminal client's session to its next mode, which
// the status line's mode segment shows.
func TestShiftTabCyclesTheModes(t *testing.T) {
	r := startRig(t, &steps{}, policy.Rules{}, nil, func(f *tuiFront) {
		f.info.Status = &config.Status{Segments: []string{"session", "mode"}}
	}, true)
	r.waitOutput("default")
	r.type_("\x1b[Z")
	r.waitOutput("acceptEdits")
	if m, _ := r.sess.Mode(); m != policy.ModeAcceptEdits {
		t.Errorf("mode %q after Shift-Tab", m)
	}
	r.type_("\x1b[Z")
	r.waitOutput("dontAsk")
	r.quit()
}
