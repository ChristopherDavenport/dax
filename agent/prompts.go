package agent

// Prompt templates: a markdown file in ~/.dax/prompts or the project's
// .dax/prompts is a slash command, /<name> args, that sends the file's
// text with its arguments filled in. The text is not the person's own
// words, so the message is marked with where it came from
// (agentturn.WithProvenance): the record writes the file as the
// message's instructions_part, and a project's as untrusted, a
// repository's text sent with the person's authority.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// Commands are the names of dax's own slash commands, which no prompt
// template may take.
var Commands = []string{"help", "quit", "exit", "abort", "follow", "tools", "session", "model", "think", "mcp", "compact", "port"}

// PromptTemplate is a prompt template the session offers as
// /<Name>.
type PromptTemplate struct {
	Name string
	// Path is where it was read: ~/.dax/prompts/<name>.md, or
	// .dax/prompts/<name>.md in the project. It is the instructions
	// layer's ID on the record.
	Path string
	// Project says the repository holds it: its text is not yours, and
	// the record marks it untrusted.
	Project bool
	text    string
}

// templateSource is the instructions layer source of a template's
// message on the record.
const templateSource = "dax.prompts"

// maxTemplateBytes is the most of a template file read.
const maxTemplateBytes = 64 << 10

var templateName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// loadTemplates reads the user's templates from userDir on this
// machine (none when userDir is ""), then the project's from
// .dax/prompts through the workspace's files, ws. A user's template
// stands over a project's of the same name. A file that cannot be a
// command (a name dax's commands use, or one that is not a name), or
// cannot be read, is left out and said once.
func loadTemplates(userDir string, ws fs.FS, warn func(string)) []PromptTemplate {
	var out []PromptTemplate
	add := func(t PromptTemplate) {
		if slices.ContainsFunc(out, func(o PromptTemplate) bool { return o.Name == t.Name }) {
			return
		}
		out = append(out, t)
	}
	read := func(fsys fs.FS, dir, shown string, project bool) {
		entries, err := fs.ReadDir(fsys, dir)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				warn(fmt.Sprintf("prompt templates in %s: %v", shown, err))
			}
			return
		}
		for _, e := range entries {
			file := e.Name()
			name, ok := strings.CutSuffix(file, ".md")
			if !ok || !e.Type().IsRegular() {
				continue
			}
			p := shown + "/" + file
			switch {
			case !templateName.MatchString(name):
				warn(fmt.Sprintf("prompt template %s: a command's name is lowercase letters, digits, - and _; left out", p))
				continue
			case strings.Contains(name, "__"):
				// mcp__<server>__<prompt> is an MCP prompt's command,
				// which a template may not stand in for.
				warn(fmt.Sprintf("prompt template %s: a double underscore is an MCP prompt's command; left out", p))
				continue
			case slices.Contains(Commands, name):
				warn(fmt.Sprintf("prompt template %s: /%s is dax's own command; left out", p, name))
				continue
			}
			data, err := fs.ReadFile(fsys, path.Join(dir, file))
			switch {
			case err != nil:
				warn(fmt.Sprintf("prompt template %s: %v; left out", p, err))
				continue
			case len(data) > maxTemplateBytes:
				warn(fmt.Sprintf("prompt template %s: over %d KiB; left out", p, maxTemplateBytes>>10))
				continue
			}
			add(PromptTemplate{Name: name, Path: p, Project: project, text: string(data)})
		}
	}
	if userDir != "" {
		read(os.DirFS(filepath.Join(userDir, "prompts")), ".", "~/.dax/prompts", false)
	}
	if ws != nil {
		read(ws, ".dax/prompts", ".dax/prompts", true)
	}
	slices.SortFunc(out, func(a, b PromptTemplate) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Templates are the prompt templates the session offers, by name.
func (s *Session) Templates() []PromptTemplate { return slices.Clone(s.templates) }

// Template is the message /<name> args sends: the template's text,
// $ARGUMENTS the whole of args and $1 to $9 its words, marked with the
// file it was composed from (and, for the project's, as untrusted), so
// the record holds it as the template's and not as words you wrote.
// It is still the request you chose to send, so a fold keeps it
// verbatim, as it keeps your own messages (#173).
func (s *Session) Template(name, args string) (openresponses.Item, error) {
	i := slices.IndexFunc(s.templates, func(t PromptTemplate) bool { return t.Name == name })
	if i < 0 {
		return nil, fmt.Errorf("no prompt template /%s", name)
	}
	t := s.templates[i]
	words := strings.Fields(args)
	pairs := []string{"$ARGUMENTS", args}
	for n := 9; n >= 1; n-- {
		w := ""
		if n <= len(words) {
			w = words[n-1]
		}
		pairs = append(pairs, "$"+strconv.Itoa(n), w)
	}
	text := strings.TrimRight(strings.NewReplacer(pairs...).Replace(t.text), "\n")
	trust := &agentturn.Trust{}
	if t.Project {
		trust.Reads = []agenttool.Label{agenttool.LabelUntrusted}
	}
	item, err := agentturn.WithProvenance(openresponses.UserText(text), agentturn.Provenance{
		Layers: []agentturn.Layer{{ID: t.Path, Source: templateSource}},
		Trust:  trust,
	})
	if err != nil {
		return nil, err
	}
	// The request the person chose to send: a fold keeps it verbatim,
	// though it is composed (tasks).
	s.tasks.add([]openresponses.Item{item})
	return item, nil
}
