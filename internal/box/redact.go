package box

// This file is the redaction of a unit's secrets from what its server
// writes: the box host replaces each value its container holds before
// any of the server's output is buffered for the session, so a server
// that returns its own secret (an env tool, an error that quotes its
// token) hands the session, the model and the record a placeholder,
// "[REDACTED NAME]". The redactor is agentredact's secrets module
// (standard library only): each value in every spelling a JSON encoder
// writes it in, a line at a time, a long line split where no value
// reaches across. It is a backstop, not a guarantee: a value the server
// encodes otherwise (base64, reversed, split across messages) is not
// found.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/ChristopherDavenport/agentredact/secrets"
)

// unitSecretValues is the values of the secrets spec names as its
// container ctr holds them now, read with docker inspect, so what is
// redacted is what the container was given, whatever the box host's
// references say today. They are never logged or returned in an error.
func (c *Client) unitSecretValues(ctx context.Context, spec Spec, ctr string) (map[string]string, error) {
	if len(spec.Secrets) == 0 {
		return nil, nil
	}
	out, err := c.Runner.Docker(ctx, "inspect", "-f", "{{json .Config.Env}}", ctr)
	if err != nil {
		return nil, fmt.Errorf("unit %s: its secrets cannot be read to redact its output: %w", spec.Unit, err)
	}
	var env []string
	if err := json.Unmarshal(bytes.TrimSpace(out), &env); err != nil {
		return nil, fmt.Errorf("unit %s: its secrets cannot be read to redact its output: docker inspect gave no environment", spec.Unit)
	}
	values := map[string]string{}
	for _, kv := range env {
		name, v, ok := strings.Cut(kv, "=")
		if _, named := spec.Secrets[name]; ok && named {
			values[name] = v
		}
	}
	return values, nil
}

// StartUnitProc is StartProc of command in spec's container ctr, with
// what its server writes, standard output and error, redacted of the
// secrets the container holds; found is told of each secret replaced,
// by name. A container whose secrets cannot be read is not started.
func (c *Client) StartUnitProc(ctx context.Context, spec Spec, ctr string, command []string, stderr bool, found func(secret string, n int)) (Proc, error) {
	values, err := c.unitSecretValues(ctx, spec, ctr)
	if err != nil {
		return nil, err
	}
	p, err := c.StartProc(ctx, ctr, command, stderr)
	if err != nil {
		return nil, err
	}
	r := secrets.New(values)
	if r.Empty() {
		return p, nil
	}
	rp := &redactedProc{Proc: p, out: r.Reader(p.Stdout(), found)}
	if e := p.Stderr(); e != nil {
		rp.err = r.Reader(e, found)
	}
	return rp, nil
}

// redactedProc is a Proc whose output is read through a secrets.Redactor.
type redactedProc struct {
	Proc
	out, err io.Reader
}

func (p *redactedProc) Stdout() io.Reader { return p.out }

func (p *redactedProc) Stderr() io.Reader {
	if p.err == nil {
		return nil
	}
	return p.err
}
