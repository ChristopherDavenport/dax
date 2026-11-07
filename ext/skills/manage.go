package skills

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/ChristopherDavenport/agentskill"
	"github.com/ChristopherDavenport/agenttool"

	"github.com/ChristopherDavenport/dax/internal/private"
)

// ManageTool is the tool the model writes its own skills with: create,
// update or delete one in the user's learned directory (LearnedDir).
const ManageTool = "skill_manage"

// LearnedDir is where the skills the model writes are kept: learned
// under the user's directory (~/.dax/learned), on this machine, beside
// the user's memory, never in the workspace. It is neither the
// project's .dax/skills, a repository's text, nor ~/.dax/skills, what
// the user installed.
func LearnedDir(userDir string) string { return filepath.Join(userDir, "learned") }

// approvalsFile names, under the user's directory, the learned skills
// whose allowed-tools the person approved, each by the digest of its
// frontmatter (Approve).
const approvalsFile = "skill-approvals.json"

// approvals is the approvals file: the frontmatter digest each learned
// skill's grants were approved for, by name.
type approvals struct {
	Approved map[string]string `json:"approved"`
}

// Approval is what Approve approved: the skill's frontmatter digest and
// the allowed-tools it grants under -trust-skills.
type Approval struct {
	Digest       string
	AllowedTools string
}

// approvalsMu serialises this process's writes of an approvals file.
var approvalsMu sync.Mutex

// skillName is the Agent Skills name grammar: lower-case letters,
// digits and single hyphens, neither first nor last, at most 64 bytes.
var skillName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func validName(name string) bool { return len(name) <= 64 && skillName.MatchString(name) }

// Approve lets the learned skill name grant its allowed-tools under
// -trust-skills, as it is now: it records the digest of the skill's
// frontmatter (agentskill's FrontmatterSHA256) in the user's approvals
// file, so a later change to the frontmatter, its allowed-tools
// included, needs approving again. It returns what was approved. Only
// a person runs it, through a slash command (/skills approve); the
// model has no route to it.
func Approve(userDir, name string) (Approval, error) {
	if userDir == "" {
		return Approval{}, errors.New("no user directory, so no learned skills")
	}
	if !validName(name) {
		return Approval{}, fmt.Errorf("%q is not a skill name", name)
	}
	sk, err := agentskill.LoadDir(filepath.Join(LearnedDir(userDir), name))
	if err != nil {
		return Approval{}, fmt.Errorf("no learned skill %s: %w", name, err)
	}
	if agentskill.HasErrors(sk.Validate()) {
		return Approval{}, fmt.Errorf("learned skill %s does not validate", name)
	}
	sum := sk.FrontmatterSHA256()
	approvalsMu.Lock()
	defer approvalsMu.Unlock()
	a, err := readApprovals(userDir)
	if err != nil {
		return Approval{}, err
	}
	if a.Approved == nil {
		a.Approved = map[string]string{}
	}
	a.Approved[name] = sum
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return Approval{}, err
	}
	if err := private.SecureDir(userDir); err != nil {
		return Approval{}, err
	}
	path := filepath.Join(userDir, approvalsFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return Approval{}, err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return Approval{}, err
	}
	return Approval{Digest: sum, AllowedTools: sk.AllowedTools}, nil
}

// approved reports whether the person approved the learned skill name
// with the frontmatter digest sum.
func approved(userDir, name, sum string) bool {
	a, err := readApprovals(userDir)
	return err == nil && sum != "" && a.Approved[name] == sum
}

// readApprovals reads the approvals file; none is no approvals.
func readApprovals(userDir string) (approvals, error) {
	var a approvals
	data, err := os.ReadFile(filepath.Join(userDir, approvalsFile))
	if errors.Is(err, fs.ErrNotExist) {
		return a, nil
	}
	if err != nil {
		return a, err
	}
	if err := json.Unmarshal(data, &a); err != nil {
		return a, fmt.Errorf("%s: %w", approvalsFile, err)
	}
	return a, nil
}

// manageArgs are skill_manage's arguments.
type manageArgs struct {
	Action       string `json:"action" enum:"create,update,delete" desc:"create a new skill, update one you wrote (replacing it whole), or delete one you wrote"`
	Name         string `json:"name" desc:"The skill's name: lower-case letters, digits and hyphens, at most 64 characters"`
	Description  string `json:"description,omitempty" desc:"For create and update: what the skill does and when to use it, as the catalogue lists it"`
	Instructions string `json:"instructions,omitempty" desc:"For create and update: the skill's instructions, Markdown, which the skill tool serves"`
	AllowedTools string `json:"allowed_tools,omitempty" desc:"For create and update: the tools the skill names as needed, in the policy's grammar, such as Bash(go test:*); they grant nothing until the user approves the skill"`
}

// manager is skill_manage over the learned directory: taken lists the
// skills of the other sources, whose names it may not take, and reload
// asks the session to list the skills again.
type manager struct {
	dir    string
	taken  func() (*agentskill.Catalog, error)
	reload func()
}

// tool is skill_manage. It is control, as the skill tool is: it writes
// on this machine, under the user's directory, and asks first, as a
// write does (no rule of dax-skills allows it).
func (m *manager) tool() agenttool.Tool {
	return agenttool.New(ManageTool,
		"Write a skill of your own so a procedure you worked out is kept for later: create one, update one you wrote, or delete one. "+
			"A skill is listed in the catalogue from your next turn and found in later sessions; read it with the skill tool. "+
			"Write the instructions as a procedure another session can follow without this conversation.",
		m.run,
		agenttool.WithAnnotations(agenttool.Annotations{Title: "Write a skill", Destructive: true}),
	)
}

func (m *manager) run(ctx context.Context, a manageArgs) (string, error) {
	if !validName(a.Name) {
		return "", fmt.Errorf("name %q: a skill name is lower-case letters, digits and single hyphens, at most 64 characters", a.Name)
	}
	if cat, err := m.taken(); err != nil {
		return "", err
	} else if sk, ok := cat.Lookup(a.Name); ok {
		return "", fmt.Errorf("a skill named %s is installed already (%s); choose another name", a.Name, sk.Location)
	}
	var data []byte
	if a.Action == "create" || a.Action == "update" {
		var err error
		if data, err = encode(a); err != nil {
			return "", err
		}
	}
	if err := private.SecureDir(m.dir); err != nil {
		return "", err
	}
	root, err := os.OpenRoot(m.dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	_, statErr := root.Stat(filepath.Join(a.Name, "SKILL.md"))
	exists := statErr == nil
	switch a.Action {
	case "create":
		if exists {
			return "", fmt.Errorf("you wrote a skill named %s already; update it instead", a.Name)
		}
		if err := root.MkdirAll(a.Name, 0o700); err != nil {
			return "", err
		}
	case "update", "delete":
		if !exists {
			return "", fmt.Errorf("there is no learned skill %s to %s", a.Name, a.Action)
		}
	default:
		return "", fmt.Errorf("action %q: want create, update or delete", a.Action)
	}
	if a.Action == "delete" {
		if err := root.RemoveAll(a.Name); err != nil {
			return "", err
		}
		m.reload()
		return fmt.Sprintf("Deleted the skill %s; the catalogue leaves it out from your next turn.", a.Name), nil
	}
	// Written whole and moved into place, so a reader sees the old
	// skill or the new one, never part of it.
	tmp := filepath.Join(a.Name, ".SKILL.md.tmp")
	if err := root.WriteFile(tmp, data, 0o600); err != nil {
		return "", err
	}
	if err := root.Rename(tmp, filepath.Join(a.Name, "SKILL.md")); err != nil {
		root.Remove(tmp)
		return "", err
	}
	m.reload()
	verb := map[string]string{"create": "Created", "update": "Updated"}[a.Action]
	note := ""
	if a.AllowedTools != "" {
		note = " Its allowed-tools grant nothing until the user approves it."
	}
	return fmt.Sprintf("%s the skill %s; the catalogue lists it from your next turn.%s", verb, a.Name, note), nil
}

// encode is the SKILL.md of a, once it parses back, validates as a
// skill in a directory of its name, and its allowed-tools parse.
func encode(a manageArgs) ([]byte, error) {
	body := strings.TrimSpace(a.Instructions)
	if body == "" {
		return nil, errors.New("instructions are required: the procedure the skill holds")
	}
	data, err := agentskill.Encode(&agentskill.Skill{Name: a.Name, Description: strings.TrimSpace(a.Description), AllowedTools: strings.TrimSpace(a.AllowedTools), Body: "\n" + body + "\n"})
	if err != nil {
		return nil, err
	}
	sk, err := agentskill.Parse(data)
	if err != nil {
		return nil, err
	}
	sk.DirName = a.Name
	var errs []string
	for _, p := range sk.Validate() {
		if p.Severity == agentskill.Error {
			if p.Field != "" {
				errs = append(errs, p.Field+": "+p.Message)
			} else {
				errs = append(errs, p.Message)
			}
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("the skill does not validate: %s", strings.Join(errs, "; "))
	}
	if _, err := sk.Rules(); err != nil {
		return nil, fmt.Errorf("allowed_tools: %w", err)
	}
	return data, nil
}
