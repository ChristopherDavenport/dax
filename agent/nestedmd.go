package agent

// AGENTS.md below where the session starts. The chain in the system
// prompt runs from the repository's root down to the start directory;
// the convention has the file nearest the work win, so a package's own
// AGENTS.md reaches the model the first turn after a tool touches a
// path in or below its directory (#170): a file tool's path, or what a
// bash line reads or writes as its facts claim them (a cat, a redirect).
// The sub-agents take the same hook (extension.Env's ChildBeforeTurn). It goes in as a message the
// record holds as composed from that file (agentturn.WithProvenance,
// instructions_part with source agentsmd), never as the person's words,
// and the instructions, and the prompt cache's prefix, stay as they
// are. What was given already is read back from the transcript, the
// path each <project_instructions> names, so a resume or a fold needs
// no state of its own.

import (
	"context"
	"encoding/json"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/ChristopherDavenport/agentpolicy"

	"github.com/ChristopherDavenport/agentsmd"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/tool"
)

// pathTools are the tools whose path argument says where the work is:
// dax-coding's file tools.
var pathTools = []string{"read", "write", "edit", "ls", "glob", "grep"}

// givenPath is the path of a file a rendered chain holds.
var givenPath = regexp.MustCompile(`<project_instructions path="([^"]*)">`)

// nestedAgentsMD is the BeforeTurn hook that gives the model the
// AGENTS.md files below the start directory that apply to the paths
// the transcript's calls touched and that it has not been given: read
// with opts, through the workspace's screened files, past those of the
// start chain (start). A file tool's path is its argument; a bash
// line's are the reads and writes its facts claim names (bash, the
// analysis bash's claim makes, tool.BashSubjects), read once a call and
// kept by the call's ID, since the analysis may run git in the
// workspace.
func nestedAgentsMD(files *tool.Files, opts agentsmd.Options, start []string, bash agentpolicy.Subjects) func(context.Context, agentturn.TurnStartInfo) (openresponses.Items, error) {
	opts.Extra, opts.Root = nil, agentsMDPath
	var mu sync.Mutex
	bashDirs := map[string][]string{}
	return func(ctx context.Context, info agentturn.TurnStartInfo) (openresponses.Items, error) {
		given := slices.Clone(start)
		var dirs []string
		add := func(dir string) {
			if dir != "." && !slices.Contains(dirs, dir) {
				dirs = append(dirs, dir)
			}
		}
		for _, it := range info.Transcript {
			switch v := it.(type) {
			case *openresponses.Message:
				for _, m := range givenPath.FindAllStringSubmatch(v.Text(), -1) {
					given = append(given, m[1])
				}
			case *openresponses.FunctionCall:
				if v.Name == "bash" && bash != nil {
					key := v.CallID + "\x00" + v.Arguments
					mu.Lock()
					ds, ok := bashDirs[key]
					mu.Unlock()
					if !ok {
						ds = bashPaths(ctx, files, bash, v.Arguments)
						mu.Lock()
						bashDirs[key] = ds
						mu.Unlock()
					}
					for _, d := range ds {
						add(d)
					}
					continue
				}
				if !slices.Contains(pathTools, v.Name) {
					continue
				}
				var a struct {
					Path string `json:"path"`
				}
				if json.Unmarshal([]byte(v.Arguments), &a) != nil || a.Path == "" {
					continue
				}
				rel, err := files.Rel(a.Path)
				if err != nil || rel == "." {
					continue
				}
				dir := rel
				if v.Name == "read" || v.Name == "write" || v.Name == "edit" {
					dir = path.Dir(rel)
				}
				add(dir)
			}
		}
		var add2 []agentsmd.File
		for _, d := range dirs {
			res, err := agentsmd.Chain(d, opts)
			if err != nil {
				// A directory the walk cannot read (gone, or a link out
				// of the workspace the screening refuses) adds nothing.
				continue
			}
			for _, f := range res.Files {
				if !slices.Contains(given, f.Path) && !slices.ContainsFunc(add2, func(a agentsmd.File) bool { return a.Path == f.Path }) {
					add2 = append(add2, f)
				}
			}
		}
		if len(add2) == 0 {
			return nil, nil
		}
		layers := make([]agentturn.Layer, len(add2))
		for i, f := range add2 {
			layers[i] = agentturn.Layer{ID: f.Path, Source: agentsmd.PartID}
		}
		text := "The project's instructions for the directories this work reached, nearest last:\n" + strings.TrimRight(agentsmd.Render(add2), "\n")
		item, err := agentturn.WithProvenance(openresponses.UserText(text), agentturn.Provenance{Layers: layers})
		if err != nil {
			return nil, err
		}
		return openresponses.Items{item}, nil
	}
}

// bashPaths are the directories a bash line's facts claim it reads or
// writes in, relative to the workspace's root: a file's directory, or
// the directory itself. A line the analysis cannot read, or a path out
// of the workspace, gives none.
func bashPaths(ctx context.Context, files *tool.Files, bash agentpolicy.Subjects, args string) []string {
	subjects, err := bash(ctx, json.RawMessage(args))
	if err != nil {
		return nil
	}
	var dirs []string
	for _, sub := range subjects {
		if sub.Tool != "read" && sub.Tool != "write" && sub.Tool != "edit" {
			continue
		}
		var a struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(sub.Args, &a) != nil || a.Path == "" {
			continue
		}
		rel, err := files.Rel(a.Path)
		if err != nil || rel == "." {
			continue
		}
		dir := path.Dir(rel)
		if fi, err := files.Stat(rel); err == nil && fi.IsDir() {
			dir = rel
		}
		if dir != "." && !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}
