package dax

import (
	"context"
	"os"
	"os/user"
	"strings"
	"sync"

	"github.com/ChristopherDavenport/agentconsole/console"
	"github.com/ChristopherDavenport/agentturn"

	"github.com/ChristopherDavenport/dax/internal/render"
	"github.com/ChristopherDavenport/dax/tool"
)

// statusSession is what the status line reads of a session: its
// repository, the model it runs next, and its events, after each tool
// run of which the repository is read again.
type statusSession interface {
	Git(ctx context.Context) (tool.Git, error)
	Model() string
	Mode() (mode, pending string)
	Subscribe(fn func(context.Context, agentturn.Event) error) func()
}

// statusHost is dax's side of the terminal client's status line
// (console.WithStatus, from the config's status block): the host
// segments user, dir, git, executor and mode, the model dax runs next, and
// the window it believes each model has. The git segment is read
// through the session's workspace, cached, and read again after each
// tool run, never as the line is drawn.
type statusHost struct {
	info    frontInfo
	sess    statusSession
	updates chan struct{}
	// kick asks the refresher to read the repository again.
	kick chan struct{}

	mu  sync.Mutex
	git tool.Git
}

func newStatus(info frontInfo, sess statusSession) *statusHost {
	return &statusHost{info: info, sess: sess, updates: make(chan struct{}, 1), kick: make(chan struct{}, 1)}
}

// line is the console's status line: the config's make-up and dax's
// host segments.
func (h *statusHost) line() console.StatusLine {
	l := h.info.Status.Line()
	user := userAtHost()
	dir := render.Clean(tildeHome(h.info.Dir, h.info.Executor == "" && h.info.Workspace == ""))
	l.Host = map[string]func() console.Segment{
		"user": func() console.Segment { return console.Segment{Text: user, Color: h.color("user", "0")} },
		"dir":  func() console.Segment { return console.Segment{Text: dir, Color: h.color("dir", "4")} },
		"git":  h.gitSegment,
		"mode": h.modeSegment,
		"executor": func() console.Segment {
			if b := elsewhereBanner(h.info); b != "" {
				_, where, _ := strings.Cut(b, ": ")
				return console.Segment{Text: render.Clean(where)}
			}
			return console.Segment{Text: "here"}
		},
	}
	l.Model = h.sess.Model
	l.Window = func(model string) int { return int(h.info.Windows[model]) }
	l.Updates = h.updates
	return l
}

// gitSegment is the branch, with ± when the tree is dirty: green clean,
// yellow dirty, unless the config colors it; nothing outside a
// repository.
func (h *statusHost) gitSegment() console.Segment {
	h.mu.Lock()
	g := h.git
	h.mu.Unlock()
	color := func(c string) string { return h.color("git", c) }
	switch {
	case !g.Repo:
		return console.Segment{}
	case g.Withheld != "":
		return console.Segment{Text: "git not read: " + render.Clean(g.Withheld), Color: color("3")}
	case g.Branch == "":
		return console.Segment{}
	case g.Dirty:
		return console.Segment{Text: render.Clean(g.Branch) + " ±", Color: color("3")}
	}
	return console.Segment{Text: render.Clean(g.Branch), Color: color("2")}
}

// modeSegment is the permission mode, and the one waiting for the
// run's next turn: magenta for acceptEdits, yellow for dontAsk, red for
// bypassPermissions, unless the config colors it.
func (h *statusHost) modeSegment() console.Segment {
	mode, pending := h.sess.Mode()
	text := mode
	if pending != "" {
		text += " → " + pending
	}
	return console.Segment{Text: render.Clean(text), Color: h.color("mode", map[string]string{"acceptEdits": "5", "dontAsk": "3", "bypassPermissions": "1"}[mode])}
}

// color is a host segment's own color, def, or "" when the config
// colors the segment, since a host segment's own color comes first.
// The defaults are an agnoster prompt's: user@host on black, the
// directory on blue, the branch green or yellow.
func (h *statusHost) color(name, def string) string {
	if _, ok := h.info.Status.Colors[name]; ok {
		return ""
	}
	return def
}

// refresh reads the repository again and redraws the line when it
// changed. An error keeps what was read before.
func (h *statusHost) refresh(ctx context.Context) {
	g, err := h.sess.Git(ctx)
	if err != nil {
		return
	}
	h.mu.Lock()
	changed := g != h.git
	h.git = g
	h.mu.Unlock()
	if changed {
		h.redraw()
	}
}

// redraw asks the client to draw the line again, dropped when one is
// already waiting.
func (h *statusHost) redraw() {
	select {
	case h.updates <- struct{}{}:
	default:
	}
}

// run reads the repository now and after each tool run, and redraws
// the line at each turn's start and each run's end, when a mode asked
// for during the run comes into force, until ctx ends; it is started
// only when the line shows git or mode.
func (h *statusHost) run(ctx context.Context) {
	git := h.info.Status.Shows("git")
	defer h.sess.Subscribe(func(_ context.Context, ev agentturn.Event) error {
		switch ev.(type) {
		case *agentturn.TurnStart:
			h.redraw()
		case *agentturn.ToolEnd, *agentturn.RunEnd:
			h.redraw()
			if git {
				select {
				case h.kick <- struct{}{}:
				default:
				}
			}
		}
		return nil
	})()
	if git {
		h.refresh(ctx)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.kick:
			h.refresh(ctx)
		}
	}
}

// userAtHost is user@host for the user segment: who runs dax, here.
func userAtHost() string {
	name := os.Getenv("USER")
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	host, _ := os.Hostname()
	host, _, _ = strings.Cut(host, ".")
	return render.Clean(name + "@" + host)
}

// tildeHome is dir with this machine's home shown as ~, when the tools
// act here; a path on another machine is shown as it is.
func tildeHome(dir string, here bool) string {
	home, err := os.UserHomeDir()
	if !here || err != nil || home == "" {
		return dir
	}
	if dir == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(dir, home+string(os.PathSeparator)); ok {
		return "~" + string(os.PathSeparator) + rest
	}
	return dir
}
