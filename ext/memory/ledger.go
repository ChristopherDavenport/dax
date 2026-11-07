package memory

// The passes' ledger: which sessions are memory passes, and what each
// pass wrote, by its range of the journal, so a pass edits only the
// entries passes wrote (Change.Session) and /memory undo reverts the
// latest pass.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentmemory/filestore"

	"github.com/ChristopherDavenport/dax/internal/private"
)

// ledgerName is the ledger, beside the store's journal: a dotfile,
// which the store does not read as a scope.
const ledgerName = ".dax-passes.jsonl"

// ledgerLine is one line of the ledger: a pass that wrote, with its
// session, the project scope it ran for and its range of the journal,
// or, with Undone set, the undo of the pass whose session and From it
// names.
type ledgerLine struct {
	Session string `json:"session"`
	Project string `json:"project,omitempty"`
	From    uint64 `json:"from"`
	To      uint64 `json:"to,omitempty"`
	Undone  bool   `json:"undone,omitempty"`
}

func appendLedger(dir string, l ledgerLine) error {
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, ledgerName), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("memory: the passes' ledger: %w", err)
	}
	_, err = f.Write(append(b, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func readLedger(dir string) ([]ledgerLine, error) {
	f, err := os.Open(filepath.Join(dir, ledgerName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("memory: the passes' ledger: %w", err)
	}
	defer f.Close()
	var out []ledgerLine
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var l ledgerLine
		if json.Unmarshal(sc.Bytes(), &l) == nil && l.Session != "" {
			out = append(out, l)
		}
	}
	return out, sc.Err()
}

// ledgerSessions are the sessions the ledger names as passes.
func ledgerSessions(dir string) (map[string]bool, error) {
	ls, err := readLedger(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, l := range ls {
		out[l.Session] = true
	}
	return out, nil
}

// Undo reverts the latest background pass of project (WorkspaceScope)
// not undone yet, in the store in dir: each entry it changed goes back
// to what it held before the pass, unless something changed it since,
// which is left and named. It returns what it did, in a line; a second
// Undo reverts the pass before. The undo is the person's, attributed to
// no session in the journal.
func Undo(ctx context.Context, dir string, project agentmemory.Scope) (string, error) {
	if err := private.SecureDir(dir); err != nil {
		return "", fmt.Errorf("memory: %w", err)
	}
	ls, err := readLedger(dir)
	if err != nil {
		return "", err
	}
	undone := map[string]bool{}
	for _, l := range ls {
		if l.Undone {
			undone[fmt.Sprint(l.Session, "/", l.From)] = true
		}
	}
	var pass *ledgerLine
	for i := len(ls) - 1; i >= 0; i-- {
		l := ls[i]
		if !l.Undone && l.Project == string(project) && !undone[fmt.Sprint(l.Session, "/", l.From)] {
			pass = &l
			break
		}
	}
	if pass == nil {
		return "no memory pass of this project's is left to undo", nil
	}
	mem, err := filestore.Open(dir)
	if err != nil {
		return "", fmt.Errorf("memory: %w", err)
	}
	type key struct {
		scope agentmemory.Scope
		name  string
	}
	// Each entry the pass changed: what it held before (the first
	// change's Replaced) and after (the last change's hash).
	type span struct{ before, after string }
	var order []key
	spans := map[key]*span{}
	// history is every hash's content, before the pass, to restore.
	history := map[string]agentmemory.Entry{}
	for c, err := range mem.Journal(ctx, 0) {
		if err != nil {
			continue
		}
		k := key{c.Entry.Scope, c.Entry.Name}
		switch {
		case c.Seq <= pass.From:
			if !c.Entry.Deleted {
				history[c.Entry.Hash] = c.Entry
			}
		case c.Seq <= pass.To && c.Session == pass.Session:
			sp := spans[k]
			if sp == nil {
				sp = &span{before: c.Replaced}
				spans[k] = sp
				order = append(order, k)
			}
			sp.after = c.Entry.Hash
			if c.Entry.Deleted {
				sp.after = ""
			}
		}
	}
	var did, left []string
	for _, k := range order {
		sp := spans[k]
		cur, err := mem.Get(ctx, k.scope, k.name)
		have := ""
		switch {
		case err == nil:
			have = cur.Hash
		case !errors.Is(err, agentmemory.ErrNotFound):
			return "", err
		}
		ref := string(k.scope) + "/" + k.name
		if have != sp.after {
			left = append(left, ref)
			continue
		}
		switch {
		case sp.before == "" && have == "":
		case sp.before == "":
			if _, err := mem.Forget(ctx, k.scope, k.name); err != nil {
				return "", err
			}
			did = append(did, "forgot "+ref)
		default:
			prev, ok := history[sp.before]
			if !ok {
				left = append(left, ref)
				continue
			}
			if _, err := mem.Put(ctx, agentmemory.Entry{Scope: k.scope, Name: k.name, Content: prev.Content, Meta: prev.Meta}, agentmemory.IfHash(have)); err != nil {
				if errors.Is(err, agentmemory.ErrConflict) {
					left = append(left, ref)
					continue
				}
				return "", err
			}
			did = append(did, "restored "+ref)
		}
	}
	if err := appendLedger(dir, ledgerLine{Session: pass.Session, Project: pass.Project, From: pass.From, Undone: true}); err != nil {
		return "", err
	}
	out := "undid the latest memory pass"
	if len(did) > 0 {
		out += ": " + strings.Join(did, ", ")
	}
	if len(left) > 0 {
		out += "; left " + strings.Join(left, ", ") + ", changed since"
	}
	return out, nil
}
