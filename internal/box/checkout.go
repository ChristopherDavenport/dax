package box

// This file is the box host's checkouts: a repository a session names,
// cloned into a directory the box host keeps and kept current only when
// nothing of a session's would be lost; and the git bundle that takes a
// project's head back to the session's machine. Git runs here, on the
// box host, with its own environment and credentials, never in a box.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// Checkouts is the box host config's checkouts: which repositories a
// session may name, and where they are kept.
type Checkouts struct {
	// Allow are glob patterns (path.Match) on a repository's URL as
	// NormalizeURL writes it; none refuses every checkout.
	Allow []string `json:"allow,omitempty"`
	// Dir is where checkouts are kept; default <user cache>/dax/box-checkouts.
	Dir string `json:"dir,omitempty"`
}

// MaxBundle is the largest bundle dax/box.pull gives.
const MaxBundle = 256 << 20

// NormalizeURL is raw as checkouts name it: scheme https, ssh or file,
// the scheme and host in lower case, a git@host:path spelling written as
// ssh://git@host/path, and a trailing "/" or ".git" dropped. A URL that
// carries credentials is refused, a password anywhere or any user on
// https (a token in a URL is a credential): the box host's own git
// config, ssh agent or credential helper sign in, never the URL.
func NormalizeURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("an empty URL")
	}
	if strings.ContainsAny(s, " \t\r\n\x00") || strings.HasPrefix(s, "-") {
		return "", fmt.Errorf("%q: not a repository URL", raw)
	}
	if !strings.Contains(s, "://") {
		// [user@]host:path, as git and ssh write it.
		colon := strings.IndexByte(s, ':')
		slash := strings.IndexByte(s, '/')
		if colon <= 0 || (slash >= 0 && slash < colon) {
			return "", fmt.Errorf("%q: want https://, ssh://, file:// or user@host:path", raw)
		}
		s = "ssh://" + s[:colon] + "/" + strings.TrimPrefix(s[colon+1:], "/")
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("%q: %w", raw, err)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", fmt.Errorf("%q: a repository URL takes no query or fragment", raw)
	}
	scheme := strings.ToLower(u.Scheme)
	user := ""
	if u.User != nil {
		if _, ok := u.User.Password(); ok {
			return "", fmt.Errorf("%q: a password in the URL; the box host's git credentials sign in, not the URL", Redact(raw))
		}
		user = u.User.Username()
	}
	host := strings.ToLower(u.Host)
	switch scheme {
	case "https":
		if u.User != nil {
			return "", fmt.Errorf("%q: a user in an https URL is a credential; the box host's git credentials sign in, not the URL", Redact(raw))
		}
		if host == "" {
			return "", fmt.Errorf("%q: no host", raw)
		}
	case "ssh":
		if host == "" {
			return "", fmt.Errorf("%q: no host", raw)
		}
		if user != "" && !sshUser.MatchString(user) {
			return "", fmt.Errorf("%q: user %q", raw, user)
		}
	case "file":
		if u.User != nil || (host != "" && host != "localhost") {
			return "", fmt.Errorf("%q: a file URL names a path on the box host, no host", raw)
		}
		host = ""
	default:
		return "", fmt.Errorf("%q: want https://, ssh://, file:// or user@host:path", raw)
	}
	p := u.Path
	if p == "" || p == "/" {
		return "", fmt.Errorf("%q: no repository path", raw)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." {
			return "", fmt.Errorf("%q: a path with %q", raw, seg)
		}
	}
	p = path.Clean(p)
	p = strings.TrimSuffix(strings.TrimRight(p, "/"), ".git")
	if p == "" || p == "/" {
		return "", fmt.Errorf("%q: no repository path", raw)
	}
	out := scheme + "://"
	if user != "" {
		out += user + "@"
	}
	return out + host + p, nil
}

var sshUser = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)

// Redact is s, a URL, without what it says before an "@" in its
// authority: what a URL must never print, a user, a password or a token.
func Redact(s string) string {
	scheme, rest, ok := strings.Cut(s, "://")
	if !ok {
		return s
	}
	auth, tail, _ := strings.Cut(rest, "/")
	if i := strings.LastIndexByte(auth, '@'); i >= 0 {
		auth = auth[i+1:]
	}
	if tail != "" || strings.Contains(rest, "/") {
		return scheme + "://" + auth + "/" + tail
	}
	return scheme + "://" + auth
}

// normalizePattern is an allow pattern written as NormalizeURL writes a
// URL, as far as a pattern can be: git@host:path read as ssh, a
// trailing "/" or ".git" dropped, the scheme and host in lower case.
func normalizePattern(p string) string {
	p = strings.TrimSpace(p)
	if !strings.Contains(p, "://") {
		if colon := strings.IndexByte(p, ':'); colon > 0 {
			if slash := strings.IndexByte(p, '/'); slash < 0 || colon < slash {
				p = "ssh://" + p[:colon] + "/" + strings.TrimPrefix(p[colon+1:], "/")
			}
		}
	}
	p = strings.TrimSuffix(strings.TrimRight(p, "/"), ".git")
	if scheme, rest, ok := strings.Cut(p, "://"); ok {
		auth, tail, found := strings.Cut(rest, "/")
		// The host in lower case, as NormalizeURL writes it; a user as
		// written.
		if i := strings.LastIndexByte(auth, '@'); i >= 0 {
			auth = auth[:i+1] + strings.ToLower(auth[i+1:])
		} else {
			auth = strings.ToLower(auth)
		}
		p = strings.ToLower(scheme) + "://" + auth
		if found {
			p += "/" + tail
		}
	}
	return p
}

// check refuses an allow pattern path.Match cannot read, and a dir that
// is not absolute.
func (c Checkouts) check() error {
	for _, p := range c.Allow {
		if strings.TrimSpace(p) == "" {
			return errors.New("checkouts.allow: an empty pattern")
		}
		if _, err := path.Match(normalizePattern(p), ""); err != nil {
			return fmt.Errorf("checkouts.allow %q: %w", p, err)
		}
	}
	if c.Dir != "" && !filepath.IsAbs(c.Dir) {
		return fmt.Errorf("checkouts.dir %q: want an absolute path", c.Dir)
	}
	return nil
}

// Allows reports whether norm, a URL as NormalizeURL writes it, is one
// of c's patterns'.
func (c Checkouts) Allows(norm string) bool {
	for _, p := range c.Allow {
		if ok, _ := path.Match(normalizePattern(p), norm); ok {
			return true
		}
	}
	return false
}

// Path is where the checkout of norm is kept: under c's dir, the
// repository's name and the first twelve hex digits of the SHA-256 of
// norm, so two repositories of one name are two checkouts.
func (c Checkouts) Path(norm string) (string, error) {
	dir := c.Dir
	if dir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("checkouts: %w", err)
		}
		dir = filepath.Join(cache, "dax", "box-checkouts")
	}
	sum := sha256.Sum256([]byte(norm))
	return filepath.Join(dir, RepoName(norm)+"-"+hex.EncodeToString(sum[:])[:12]), nil
}

// RepoName is the last segment of a repository URL's path, as a
// directory or a branch may spell it: lower case, letters, digits, '.',
// '_' and '-', at most 40; "repo" when nothing is left.
func RepoName(u string) string {
	seg := u
	if i := strings.LastIndexAny(seg, "/:"); i >= 0 {
		seg = seg[i+1:]
	}
	seg = strings.TrimSuffix(seg, ".git")
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		}
		return '-'
	}, seg)
	if len(name) > 40 {
		name = name[:40]
	}
	name = strings.Trim(name, "-.")
	if name == "" {
		return "repo"
	}
	return name
}

// CheckRef refuses a ref a checkout cannot name: a branch or a tag,
// letters, digits, '.', '_', '/' and '-', not starting with '-' or '/',
// with no "..", "//" or trailing "/" or ".lock".
func CheckRef(field, ref string) error {
	switch {
	case !refName.MatchString(ref), strings.Contains(ref, ".."), strings.Contains(ref, "//"),
		strings.HasSuffix(ref, "/"), strings.HasSuffix(ref, ".lock"), strings.HasSuffix(ref, "."):
		return fmt.Errorf("%s %q: want a branch or a tag", field, ref)
	}
	return nil
}

var refName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/-]*$`)

// CheckoutState is a checkout as the box host finds or leaves it.
type CheckoutState struct {
	// URL is the repository, as NormalizeURL wrote it; Ref the branch or
	// tag the session asked for, empty for the remote's default.
	URL  string `json:"url"`
	Ref  string `json:"ref,omitempty"`
	Path string `json:"path"`
	// Branch is the branch checked out, empty when detached; Head its
	// commit; Dirty whether the worktree has changes not committed.
	Branch string `json:"branch,omitempty"`
	Head   string `json:"head"`
	Dirty  bool   `json:"dirty,omitempty"`
	// Note is what the box host did, or why it left the worktree alone.
	Note string `json:"note,omitempty"`
}

// git runs git in dir with the box host's environment, never asking
// for a password (there is no terminal to ask on).
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", args[0], tail(msg, 1000))
	}
	return strings.TrimSpace(string(out)), nil
}

// SyncCheckout gives the checkout of norm at dir, current where nothing
// would be lost: cloned when it is not there; else fetched, and moved
// to ref (the remote's default branch when empty) only when its
// worktree is clean and on that ref with no commits of its own. Any
// other worktree is left as it is, and Note says why. A directory that
// holds another repository is refused, untouched.
func SyncCheckout(ctx context.Context, dir, norm, ref string) (CheckoutState, error) {
	st := CheckoutState{URL: norm, Ref: ref, Path: dir}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
			return st, fmt.Errorf("checkout: %w", err)
		}
		args := []string{"clone", "--quiet"}
		if ref != "" {
			args = append(args, "--branch", ref)
		}
		args = append(args, "--", norm, dir)
		if _, err := git(ctx, filepath.Dir(dir), args...); err != nil {
			return st, fmt.Errorf("checkout of %s: %w", norm, err)
		}
		st.Note = "cloned"
		return readInto(ctx, st)
	} else if err != nil {
		return st, fmt.Errorf("checkout: %w", err)
	}
	if origin, err := git(ctx, dir, "config", "--get", "remote.origin.url"); err != nil || origin != norm {
		return st, fmt.Errorf("checkout %s holds another repository than %s; it is left as it is", dir, norm)
	}
	if _, err := git(ctx, dir, "fetch", "--quiet", "--prune", "--tags", "origin"); err != nil {
		return st, fmt.Errorf("checkout of %s: %w", norm, err)
	}
	st.Note = "fetched; " + moveTo(ctx, dir, ref)
	return readInto(ctx, st)
}

// moveTo moves the checkout at dir to ref, a branch or a tag (the
// remote's default branch when empty), when that loses nothing, and
// says what it did or why it did not.
func moveTo(ctx context.Context, dir, ref string) string {
	if status, err := git(ctx, dir, "status", "--porcelain"); err != nil {
		return "its worktree could not be read; left as it is"
	} else if status != "" {
		return "its worktree has uncommitted changes; left as it is"
	}
	branch, _ := git(ctx, dir, "symbolic-ref", "--short", "-q", "HEAD")
	head, err := git(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return "it has no commit; left as it is"
	}
	target, kind := "", ""
	switch {
	case ref == "":
		def, err := git(ctx, dir, "rev-parse", "--abbrev-ref", "origin/HEAD")
		if err != nil {
			git(ctx, dir, "remote", "set-head", "origin", "--auto")
			if def, err = git(ctx, dir, "rev-parse", "--abbrev-ref", "origin/HEAD"); err != nil {
				return "the remote's default branch is not known; left as it is"
			}
		}
		ref, target, kind = strings.TrimPrefix(def, "origin/"), def, "branch"
	default:
		if _, err := git(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+ref); err == nil {
			target, kind = "origin/"+ref, "branch"
		} else if _, err := git(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/tags/"+ref); err == nil {
			target, kind = "refs/tags/"+ref, "tag"
		} else {
			return fmt.Sprintf("the remote has no branch or tag %s; left as it is", ref)
		}
	}
	want, err := git(ctx, dir, "rev-parse", target+"^{commit}")
	if err != nil {
		return fmt.Sprintf("%s cannot be read; left as it is", target)
	}
	switch kind {
	case "branch":
		if branch != ref {
			on := "a detached head"
			if branch != "" {
				on = "branch " + branch
			}
			return fmt.Sprintf("it is on %s, not %s; left as it is", on, ref)
		}
	case "tag":
		if branch != "" {
			return fmt.Sprintf("it is on branch %s, not tag %s; left as it is", branch, ref)
		}
	}
	if head == want {
		return "up to date with " + target
	}
	if _, err := git(ctx, dir, "merge-base", "--is-ancestor", head, want); err != nil {
		if _, err := git(ctx, dir, "merge-base", "--is-ancestor", want, head); err == nil {
			return fmt.Sprintf("it has commits %s does not; left as it is", target)
		}
		return fmt.Sprintf("it has diverged from %s; left as it is", target)
	}
	if kind == "tag" {
		if _, err := git(ctx, dir, "checkout", "--quiet", "--detach", want); err != nil {
			return fmt.Sprintf("moving to %s failed (%v); left as it is", target, err)
		}
	} else if _, err := git(ctx, dir, "merge", "--quiet", "--ff-only", want); err != nil {
		return fmt.Sprintf("fast-forwarding to %s failed (%v); left as it is", target, err)
	}
	return "fast-forwarded to " + target
}

// ReadCheckout is the git state of the project at dir, read only:
// branch, head and whether the worktree has changes. A project that is
// not a git repository has an empty Head.
func ReadCheckout(ctx context.Context, dir string) (CheckoutState, error) {
	return readInto(ctx, CheckoutState{Path: dir})
}

func readInto(ctx context.Context, st CheckoutState) (CheckoutState, error) {
	head, err := git(ctx, st.Path, "rev-parse", "HEAD")
	if err != nil {
		return st, fmt.Errorf("checkout %s: %w", st.Path, err)
	}
	st.Head = head
	st.Branch, _ = git(ctx, st.Path, "symbolic-ref", "--short", "-q", "HEAD")
	if status, err := git(ctx, st.Path, "status", "--porcelain"); err == nil {
		st.Dirty = status != ""
	}
	return st, nil
}

// Bundle is the head of a project, as dax/box.pull gives it: a git
// bundle of HEAD, its branch and commit, and whether the worktree had
// changes, which the bundle does not hold.
type Bundle struct {
	Data   []byte `json:"data"`
	Size   int64  `json:"size"`
	Name   string `json:"name"`
	Branch string `json:"branch,omitempty"`
	Head   string `json:"head"`
	Dirty  bool   `json:"dirty,omitempty"`
}

// BundleHead is a bundle of the git repository at dir's HEAD and its
// history, refused above max bytes, with the size.
func BundleHead(ctx context.Context, dir string, max int64) (Bundle, error) {
	st, err := ReadCheckout(ctx, dir)
	if err != nil {
		return Bundle{}, fmt.Errorf("pull: the project %s is not a git repository with a commit: %w", dir, err)
	}
	tmp, err := os.MkdirTemp("", "dax-box-bundle-")
	if err != nil {
		return Bundle{}, err
	}
	defer os.RemoveAll(tmp)
	file := filepath.Join(tmp, "head.bundle")
	if _, err := git(ctx, dir, "bundle", "create", "--quiet", file, "HEAD"); err != nil {
		return Bundle{}, fmt.Errorf("pull: %w", err)
	}
	fi, err := os.Stat(file)
	if err != nil {
		return Bundle{}, err
	}
	if fi.Size() > max {
		return Bundle{}, fmt.Errorf("pull: the bundle of %s is %d bytes, more than the %d a pull takes", dir, fi.Size(), max)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return Bundle{}, err
	}
	return Bundle{Data: data, Size: fi.Size(), Branch: st.Branch, Head: st.Head, Dirty: st.Dirty}, nil
}

// FetchBundle fetches b's head into refs/heads/branch of the git
// repository at repo, after verifying the bundle: a branch already
// there moves only forward unless force, and the branch checked out in
// repo is refused, since moving it would leave its worktree behind. It
// gives the commit the branch had before ("" for a new branch).
func FetchBundle(ctx context.Context, repo string, b Bundle, branch string, force bool) (before string, err error) {
	if _, err := git(ctx, repo, "check-ref-format", "--branch", branch); err != nil {
		return "", fmt.Errorf("pull: branch %q: %w", branch, err)
	}
	if cur, _ := git(ctx, repo, "symbolic-ref", "-q", "HEAD"); cur == "refs/heads/"+branch {
		return "", fmt.Errorf("pull: %s is checked out here; pull into another branch (-branch) or check out another first", branch)
	}
	tmp, err := os.MkdirTemp("", "dax-box-pull-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	file := filepath.Join(tmp, "head.bundle")
	if err := os.WriteFile(file, b.Data, 0o600); err != nil {
		return "", err
	}
	if _, err := git(ctx, repo, "bundle", "verify", "--quiet", file); err != nil {
		return "", fmt.Errorf("pull: the bundle does not verify: %w", err)
	}
	const incoming = "refs/dax-box/incoming"
	if _, err := git(ctx, repo, "fetch", "--quiet", "--no-tags", file, "+HEAD:"+incoming); err != nil {
		return "", fmt.Errorf("pull: %w", err)
	}
	defer git(ctx, repo, "update-ref", "-d", incoming)
	got, err := git(ctx, repo, "rev-parse", incoming)
	if err != nil {
		return "", err
	}
	if got != b.Head {
		return "", fmt.Errorf("pull: the bundle holds %s, not the head %s the box host named", got, b.Head)
	}
	ref := "refs/heads/" + branch
	before, _ = git(ctx, repo, "rev-parse", "--verify", "--quiet", ref)
	if before != "" && before != got && !force {
		if _, err := git(ctx, repo, "merge-base", "--is-ancestor", before, got); err != nil {
			return before, fmt.Errorf("pull: %s has commits the box's head does not; give -force to replace it, or -branch another", branch)
		}
	}
	if _, err := git(ctx, repo, "update-ref", ref, got); err != nil {
		return before, fmt.Errorf("pull: %w", err)
	}
	return before, nil
}
