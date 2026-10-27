# A conversation host: a library above agentkit that holds many live sessions

Repository: a candidate, not yet a module. It would be a new sibling
above agentkit, incubated in dax first (see "Where it lives"). This is a
draft so the user can judge whether the library is real. Nothing here is
built, and none of the sibling issues below is filed.

Refs dax#113. Bears on PR #135 (`docs/plans/proposals/host-mode.md` on
branch `host-mode-proposal`), whose `dax host` would be its first
consumer, and on agentstudies' dexclaw (`examples/openclaw-daemon`),
whose `internal/host` was written as "the layer we suspect is a
library" and has been measured as one for eight rounds.

## What the host is and is not

The host holds the live conversations of a long-lived program. It maps
an application's key (a channel, a chat, a name the operator gave) to
at most one live session in the process, with the store's ref keeping
that mapping across restarts and across processes on one store. It makes
a conversation live through a factory the product supplies, and drives
it only through agentturn's `Control`. It holds the session while it
runs or while something holds it open (a view's stream, an A2A task),
closes it when idle, reopens it from the store on the next request, and
shuts everything down in order. It takes inputs that arrive while a run
is in flight. It serves each session's `front/control`, and whatever
else the session mounts, under one `http.Handler` with a listing. It
does not build an agent, choose a model or a policy, or decide who
answers a held call. It does not authenticate anyone, run a tunnel hub
or pick a workspace. The application does all of those, through the
factory, the session it returns, a workspace resolver and the
authenticator hook. One session is one conversation, as everywhere in
the workspace. The host never puts two conversations in one session,
and an A2A conversation under a hosted session is that session's own
business (`OpenPeer`), not a second registry entry.

## The API sketch

The package is called `agenthost` here. Names are discussed under
"Where it lives".

### The session the product builds

```go
// Session is one conversation made live by the product: its agent as
// agentturn's control contract, the session it records in, and what
// ends it. The host drives it through these methods alone, so the
// product's rules around a run are the Session's own: answering the
// calls a restart left pending before the next prompt (dexclaw asks a
// reviewer through the policy engine; dax answers with what is known
// of each call and refuses with ErrHeld while held calls wait), or
// cancelling a question nobody is there to answer.
type Session interface {
	agentturn.Control
	// ID is the session the conversation records in.
	ID() string
	// Close ends what the product opened for the conversation: its
	// kit, its MCP servers, its remote session on the workspace. The
	// host calls it once, after the last run has ended or the drain
	// has passed, and then releases the session in the store. The
	// record stays.
	Close() error
}

// Commander is a Session with controls of its own, served beside the
// loop's as front/control commands: dax's info, model, think, mcp-add,
// mcp-remove and continue-from.
type Commander interface {
	Commands() map[string]control.Command
}

// Mounter is a Session that serves more than its control. Each handler
// is mounted under the session's prefix, behind the host's holds and
// reopen, and the host does not authenticate it: front/a2a carries its
// own Authenticator, and a2a-go stays out of this module.
type Mounter interface {
	Mounts() map[string]http.Handler // "a2a/", ".well-known/agent-card.json"
}

// Busy is a Session that knows of work the host cannot see, such as
// an A2A task waiting on its caller. A busy session is not idle.
type Busy interface {
	Busy() bool
}
```

The factory returns a product `Session`, not an `agentturn.Config` and
not an `*agentkit.Kit`:

- From a `Config`, the host would have to build the agent, the recorder
  and the run context itself. That would mean writing
  `agentkit.Control` again (recorder and session ID on every run,
  `Engine.Release` before `Resume`, `Requeue`), or importing agentkit
  and taking a kit.
- From a kit, the host could call `Kit.Control` itself. But dax's
  `agent.Session` wraps that control with rules of its own (`ErrHeld`,
  answering calls cut off by an abort, `Answering`), and dexclaw needs
  its reviewer loop around it. Both would be lost, or would need hooks
  that exist for one product each.
- A `Session` is what both products already have. dax's
  `*agent.Session` is a `Control` with `ID` and `Close` today, and
  dexclaw's would be `Kit.Control` with its recovery around `Prompt`.
  For a product with no rules of its own, a five-line adapter in a
  subpackage (`kitsession.New(kit, ctl, release)`) makes one from a
  kit.

### The factory

```go
// Key names a conversation in the application's terms. The host keeps
// it as an agentsession ref, prefix + "/" + key, so it must be a valid
// ref path (agentsession.ValidRefName); Escape makes one from any
// string.
type Key string

// Opening is what the factory and the resolver are called with.
type Opening struct {
	Key Key
	// SessionID is the session the key's ref points to, followed along
	// continued_in (agentsession.ResolveCurrent), or "" for a key the
	// store has never seen, for which the factory creates a session.
	SessionID string
	// Request is the product's own open request, as Open or
	// POST /sessions received it, opaque to the host; nil on a reopen.
	// What a reopen needs, the product reads from the record it wrote at
	// the first open: its header's extra members, its first entries.
	Request json.RawMessage
	// Workspace is what the Resolver bound, zero without one.
	Workspace Bound
}

// Factory makes a conversation live. The host calls it at most once at
// a time per key, with ctx bounded by WithOpenTimeout. For a new key it
// creates the session through the product's own path (the kit's
// WithSession, which writes the header through the recorder), and the
// host then claims the key's ref by compare-and-swap. A process that
// lost the claim closes this Session, deletes the session it created,
// which nothing else has seen, and opens the winner's. For a known key
// it resumes SessionID. An agentsession.ErrSessionLocked from it is
// reported as ErrHeldElsewhere.
type Factory func(ctx context.Context, o Opening) (Session, error)
```

### The registry

```go
// New makes a host over store, which must be an agentsession.RefStore
// and release what it holds (Release(id) error).
func New(store agentsession.Store, f Factory, opts ...Option) (*Host, error)

func WithRefPrefix(prefix string) Option         // "dexclaw/conversations", "dax/host"
func WithIdle(d time.Duration) Option            // default 10m; 0 keeps sessions live
func WithMaxLive(n int) Option                   // when full, the least recently used idle session closes; none idle: ErrFull
func WithOpenTimeout(d time.Duration) Option     // bounds resolver + factory, so a workspace's wait is bounded here
func WithDrain(d time.Duration) Option           // how long Close and Shutdown wait for a run's end to be recorded
func WithResolver(r Resolver) Option             // below
func WithAutoOpen() Option                       // Dispatch and Get open an unknown key with a nil request (dexclaw); without it, ErrUnknown (dax)
func WithLogger(l *slog.Logger) Option           // every line carries key and session

// Open makes a new conversation live. ErrExists when the key's ref is
// set: a conversation already named so is reopened with Get, or forked.
func (h *Host) Open(ctx context.Context, key Key, req json.RawMessage) (Session, error)

// Get returns the key's live session, reopening it from the store when
// it is not live. ErrUnknown for a key with no ref (unless WithAutoOpen),
// ErrHeldElsewhere when another process holds its session, ErrClosed
// after Shutdown. A key whose ref resolves to a session another key has
// live is refused: one writer per session in this process.
func (h *Host) Get(ctx context.Context, key Key) (Session, error)

// Lookup is the key's session if it is live now; it never reopens.
func (h *Host) Lookup(key Key) (Session, bool)

// Hold keeps the key's session live (reopening it if needed) until
// release is called. The handler holds for every request it serves,
// for an event stream's whole length; a product holds for work the
// handler does not see.
func (h *Host) Hold(ctx context.Context, key Key) (s Session, release func(), err error)

// List is every conversation under the prefix: the live ones with
// their state, and the others from the store's refs (ListRefs).
func (h *Host) List(ctx context.Context) ([]Status, error)

// Each calls fn for every live session, outside the host's lock
// (dexclaw's SIGHUP reload of every live kit's skills).
func (h *Host) Each(fn func(Key, Session))

// Close ends a live session: aborts its run, waits the drain for its
// end to be recorded, ends the streams the handler holds on it, calls
// Session.Close and releases the session in the store. The ref and the
// record stay, and the next Get reopens it. Idle closing is this.
func (h *Host) Close(ctx context.Context, key Key) error

// Run closes idle sessions until ctx ends, then calls Shutdown.
func (h *Host) Run(ctx context.Context) error

// Shutdown, in order: refuses new opens, reopens and inputs (ErrClosed;
// the handler answers 503); aborts every run; waits the drain for each
// run's end to be recorded; ends every stream; closes every Session,
// all at once; releases every session in the store. The application
// then closes what the sessions used (a tunnel hub), and last its
// listener, which the hub's connections ride.
func (h *Host) Shutdown(ctx context.Context) error

// Status is one conversation's state.
type Status struct {
	Key       Key
	SessionID string
	Live      bool
	Running   bool
	Turn      int
	Pending   []string // each call's name and reason
	Steering  int
	FollowUps int
	Holds     int
	IdleFor   time.Duration
	Workspace *agentworkspace.Descriptor // as bound, when the resolver bound one
}
```

### Inputs while a run is in flight

```go
type Mode int

const (
	Prompt   Mode = iota // starts a run; ErrBusy while one runs or is starting
	Steer                // into the run in flight after its batch, or starts one
	FollowUp             // after the run in flight, or starts one
)

type Input struct {
	Trigger agentturn.Trigger
	Items   openresponses.Items
	Mode    Mode
}

type Result struct {
	SessionID string
	Queued    bool              // joined a run in flight
	End       *agentturn.RunEnd // when it started one and it ended before ctx
}

// Dispatch delivers one input to a conversation, through its Session.
// The run's context is the host's, carrying the trigger, never the
// caller's: a caller that goes away (an HTTP client that disconnects)
// stops waiting and does not abort an unattended run (dexclaw H10).
// The host marks the session as starting under its lock before it calls
// Prompt, so a second input cannot start a second run in the window
// before State().Running is true, and queues behind it instead (H4).
// A queued input is the Session's Queue, which a kit's control writes
// as a queued entry before it returns, so it survives a crash
// (Recorder.Requeue at the reopen).
func (h *Host) Dispatch(ctx context.Context, key Key, in Input) (Result, error)
```

One race stays until agentturn has the fix below: a steer accepted just
as the run passes its last drain waits for the next run.
`Agent.Deliver` closes that window, but it is not on the `Control`
contract.

### The handler

```go
// Handler serves the host. Nothing is authenticated by the library:
// every route but a Mounter's is put to the application's
// Authenticator, and a handler without one refuses every request.
func (h *Host) Handler(opts ...HandlerOption) http.Handler

func WithAuthenticator(a Authenticator) HandlerOption
func WithChallenge(value string) HandlerOption // the WWW-Authenticate of every 401, as front/control's

type Authenticator func(r *http.Request) (Principal, error)

type Principal struct {
	Name string
	// Scopes are the host's (list, open, close) and front/control's
	// (read, control, answer, command), which the host passes through
	// to each session's control handler as its principal.
	Scopes []string
	// Sessions limits the principal to some keys; nil is every key.
	Sessions func(Key) bool
}
```

| Route | Scope | |
|---|---|---|
| `GET /sessions` | list | `List`, filtered by `Principal.Sessions` |
| `POST /sessions` | open | `{"key": ..., "request": {...}}` → `Open`; answers the key, the session ID and its prefix |
| `GET /sessions/{key}` | list | one `Status` |
| `DELETE /sessions/{key}` | close | `Close` |
| `/s/{key}/control/` | front/control's | `control.Handler(session, WithCommands(session.Commands()))` with the host's principal, built at each open, held for each request, reopened on demand |
| `/s/{key}/<mount>` | none here | each `Mounter` handler, held and reopened like control; it authenticates itself |

The store's read surface (agentsession's `remote`) is not mounted by
the host. Its principals and scopes are the application's, and it serves
the whole store, not the live sessions, so the application mounts it
beside the host's handler.

### The workspace

```go
// Resolver binds a conversation to the workspace its tools act in. It
// is the application's: a directory under an allowed root, a box, or
// a name on a tunnel hub (hub.Workspace(ctx, name), which already waits
// for the name to connect as long as ctx allows). The host calls it
// before the factory, under WithOpenTimeout, and passes what it bound
// in Opening.Workspace. On a reopen, Opening.SessionID lets it read the
// workspace the header recorded and bind that one.
type Resolver func(ctx context.Context, o Opening) (Bound, error)

type Bound struct {
	Workspace agentworkspace.Workspace
	// Lost is closed when the workspace the session was bound to is
	// gone or replaced, as when a tunnel name is taken by a newer
	// connection. Nil never closes.
	Lost <-chan struct{}
}

// WithOnLost says what the host does when a Bound's Lost closes. The
// default closes the session once its run has ended, so the next
// request reopens it and the resolver binds again. The factory compares
// what is bound with what the header recorded and refuses a different
// root or grant (dax#113 decision 3: no switching).
func WithOnLost(fn func(key Key, s Session)) Option
```

The workspace is the session's, and `Session.Close` ends what the
session opened on it. The host only orders it: no session outlives the
host's `Shutdown`, so a hub the application closes after `Shutdown`
has nothing left to serve.

## Mapping

### dexclaw's `internal/host`

`host.go` is 574 lines, 415 of them logic (no blank or comment-only
lines), at agentkit v0.0.7.

| Part | Goes to | Why |
|---|---|---|
| `Mode`, `Input`, `Result`, `ErrBusy`; `acquire`'s busy check, `starting`, `finish` | library (`Dispatch`) | the busy window (H4) and the three modes are any gateway's. dax's views send the same three and need the same refusal |
| `Dispatch`'s queue branch | library; agentturn (Deliver on `Control`) | the remaining race is the loop's to close |
| the request context used as the run's (api.go, H10) | library | `Dispatch` runs on the host's context |
| `ContextWithTrigger` | library (from `Input.Trigger`) | |
| `agentmemory.WithSession`, `session.ContextWithSessionID` on the run; `Recorder.Requeue` at hydrate | agentkit (`Kit.Control`, released since dexclaw's v0.0.7) | the kit's control puts both on every run and hands back owed inputs. This is a version bump, not the library |
| `recover` (`Engine.Answers` after a restart, the `RunEnd` it builds) and `review` (the nobody-reviewer loop) | dexclaw's `Session`, around `Kit.Control.Prompt`; agentkit if the issue below lands | who answers a held call is a product's policy. dax answers differently |
| `Store` (`Store` plus `Release`), `ErrLocked` | agentsession | `ErrSessionLocked` is in the root already; `Release` is still asserted by shape |
| `hydrate`'s key map, the create under its file lock, the `info` name | agentsession refs (#129, closed), used by the library | `SessionFor`'s compare-and-swap is that lock |
| `hydrate`'s kit, `agentturn.New`, `Attach`, logs | dexclaw's factory | the product's assembly |
| `inspect`, `Options.Reap` | dexclaw's factory | which process group to kill is the tool's |
| `Options.Attach` (the deliverer) | dexclaw's factory, subscribing before it returns | |
| `Options.Reviewer` | dexclaw | |
| `Status`, `SessionID` | library (`Status`, `Lookup`) | |
| `ReloadSkills` | dexclaw, over `Each` | a kit's own operation |
| `Run`, `evict`, `shutdown`, `drop`, `release` | library | |
| `UpdateKeys`, and its use in `-rollover` | deleted: agentsession refs; rollover is `ContinueRef` | |

What `host.go` shrinks to: of the 415 logic lines, about 195 are the
library's (types and modes, the registry, the busy window, status, the
idle loop, shutdown) and about 58 are agentsession refs, which dexclaw
could take today without the library. About 8 go to `Kit.Control` on a
current agentkit. What stays is dexclaw's factory and its unattended
`Session`: about 135 logic lines, some 190 with comments, in place of
574. If agentkit takes the unattended control (`recover`, `review` and
the answers branch of `Dispatch`), it is about 90 logic lines, some 130
with comments. `internal/api` and `internal/clock` change only the
package they name for `Input` and the modes.

### dax's host proposal (PR #135)

| Part of `host-mode.md` | Goes to | Why |
|---|---|---|
| `dax host`, `host.json`, `dirs`, `default_workspace` | dax | the product's config |
| the three tokens, `internal/authn`, taking tokens from the environment | dax | libraries ship no auth; the host takes `WithAuthenticator` and `WithChallenge` |
| `POST`/`GET`/`DELETE /host/sessions` | library routes | `/sessions` mounted under `/host` |
| the open request as a tighten-only config layer, its refusals | dax, in the factory | a product's config rules |
| `name`: a DNS label, unique on the host | library (the key, a ref) plus dax's own check | refs are unique by compare-and-swap |
| `/s/{id}/control/` with `serveCommands` | library mounts `front/control`; the commands are dax's (`Commander`) | |
| `/s/{id}/a2a`, its card, `a2aPeers`, `a2aStore`, `OpenPeer` | library mounts and holds (`Mounter`, `Busy`); the executor, its store and its recorder are dax's | |
| `/store/` | dax mounts agentsession's `remote` beside | its scopes are the application's |
| `GET /host/workspaces` | dax: `Hub.Connected()` and the library's `Status.Workspace` | |
| `max_sessions`, `idle`; idle close; reopen on demand; URLs that hold across both | library | |
| restart: the private directory of opened sessions | library (refs) and dax (the open request in the header's extra members) | the private directory goes; the A2A directory stays |
| shutdown order | library for the sessions; dax for the hub, then the listener | |
| hub names, `Dax-Workspace-Name`, retiring a replaced instance | dax (`authn.Hub`) over agentworkspace's tunnel | newest-replaces is already the tunnel's default |
| a replacement seen by a session: hold, re-dial, re-check, `dax.workspace.replaced` | dax, on `Bound.Lost`; agentworkspace (the replaced signal) | `WithOnLost`'s default (close, reopen, re-check) is the simpler form of the same rule |
| waiting for a connected workspace | library bounds it (`WithOpenTimeout`); agentworkspace already waits on ctx | |
| `servers_workspace.connected`, `checkServersWorkspace` | dax, in the resolver and the factory | |
| step 2's factoring: the session's build, the served session under a prefix | dax builds the session; the served-under-a-prefix half is the library's handler | |
| `viewed`, `Answering`, `SubscribeView` | dax's `Session`; the library's stream holds stand in for "a view attached" when idling | |
| notes prefixed with the session ID | library (`WithLogger`) | |
| `dax host ls|open|close`, `dax attach -session` | dax; the library could ship a client of `/sessions` | |

What moves out of dax: the registry, idle close and reopen, the
restart directory, the session routes and their mounting, holds and
shutdown order. That is most of step 3 and the lifecycle half of step 5.
What stays: the config, the tokens, the open request's rules, the hub
and its enrollment, the workspace re-check and every security note.

## Gaps in the siblings

Candidate issues, not filed. The lead's guess "tunnel: wait for a name
with ctx" is not a gap: `Hub.Workspace(ctx, name)` already waits for
the name while ctx allows.

**agentworkspace: tunnel: say when a name's connection is replaced, and
offer a workspace held to its connection.** A `Hub` replaces a name's
connection when a newer one arrives, and the requests of a
`remote.Workspace` dialled before go to the new host while the workspace
still describes the old one. The doc tells a program to dial again and
not to write through the old workspace, but gives it no way to learn
that a replacement happened (the hub's `changed` channel is internal).
Proposed: a per-name signal (`Hub.Replaced(name) <-chan struct{}`, or a
`WithOnReplace(func(name string))` option) and a dial option that holds
a workspace to the connection it was dialled over, failing its requests
with `ErrReplaced` after a replacement. A host closes or re-checks a
session on the first. The second makes dax's "hold, re-dial" PR #135's
own follow-up, and makes a stale write impossible rather than merely
documented.

**agentsession: name the `Release` a holding store offers.** `cas`,
`remote.Client` and others free a session with `Release(id string)
error`, but `Store` does not name it. agentkit's docs and `remote`'s
lease code assert `interface{ Release(string) error }` by shape,
dexclaw declares a `Store` of its own to name it, and #257 shows the
cost: `sqlite`'s `Release` returns nothing, so
every such assertion fails there and nothing is let go. Proposed: a
`Releaser` interface in the root, implemented by every store that holds
sessions, with `storetest` cases. A host that idles sessions out calls
it on every close.

**agentsession: claim a ref for a session the caller created.**
`SessionFor(ctx, st, name, header)` creates the session itself with
`st.Create`. A product whose session is made by a recorder (the kit's
`WithSession`, which writes the header and its first entries) cannot use
it, and has to write the compare-and-swap, the loser's delete and the
retry again. Proposed: `ClaimRef(ctx, st, name, id) (winner string,
err error)`, or a `SessionFor` variant that takes a create function. It
sets the ref to `id` when absent and otherwise returns the holder, and
the caller deletes its own session.

**agentsession: make a ref name from an application key.** `ValidRefName`
allows `[A-Za-z0-9._-]` per segment and `RefConflict` refuses names that
differ only in case. A Slack thread (`C123/1700000000.123`), an e-mail
address or a user named `Chris` beside `chris` is not a ref as it
stands. Every host that keys by refs will write an escape, and two
escapes on one store would disagree. Proposed: a reversible
`RefSegment(s string) string` (and its inverse) in the root, documented
in RFC 0002's *Refs*, which encodes characters outside the set and
upper case so case-only collisions cannot arise.

**agentturn: `Deliver` on the `Control` contract and over
`front/control`.** `Agent.Deliver` joins the run in flight or starts one
and waits until a model call has seen the items. That is what a gateway
means by "steer this in, whatever the agent is doing", and it closes
the window where a steer accepted as a run passes its last drain waits
for the next input. It is not on `Control`, so a host that drives a
product's `Session`, or a `control.Client` over the wire, cannot reach
it, and has to emulate it with `Queue` plus `Prompt` and a starting flag
(dexclaw's `starting`). Proposed: `Deliver` on `Control`, implemented by
`*Agent`, by agentkit's `Control` (under its run context) and by
`front/control` (`POST /deliver`).

**agentkit: an unattended control that answers with a reviewer.** A host
with nobody at it answers what the policy asks, and the calls a restart
left pending, through `Engine.Answers` with an `agentpolicy.Reviewer`,
resuming until the run ends for another reason or the denial bound
stops it. dexclaw writes this as `recover` and `review` (39 logic
lines, plus building a `RunEnd` after a restart, whose run
`Kit.Control` already finds for its own release, `seedRun`). dax's `Drive` is the same idea answering by its
own rules. Proposed: `Kit.Control(a, agentkit.WithReviewer(r))`, or a
`Control.Settle(ctx, reviewer)` that a product's `Prompt` calls first,
from exported calls alone as the kit's rule requires.

Existing issues that bear on the host: agentturn#257 (front/a2a tasks do
not survive a restart, so reopening a session for an A2A call loses the
task), agentsession#221 (`remote` cannot say which sessions a principal
may read, which matters once the store route serves anyone but the
user), agentsession#222 (`ContinueRef` can fork a chain a `continued_in`
already retired, which a host's rollover would hit), and dax#134
(enrollment stronger than one token).

## Where it lives, its name, and who uses it

**Not in agentkit.** agentkit's rule is that `Config()` returns a plain
`agentturn.Config` and the manual path is the same path. Its README
says it is "not a UI, a server, a launcher or a deployment tool", and
`make direct` bounds its imports to the siblings and the MCP SDK. A
host holds many kits, owns goroutines, timers and an `http.Handler`, and
none of that has a "manual path" a test could compare. It sits above
the kit, not in it.

**Not yet a repository.** The workspace extracts a library once a
second product needs it. dexclaw is a study's product in a private repo,
and `dax host` is unbuilt. The precedent is dax's `internal/box`: it is
held to the standard library and two sibling packages so that "it can
move to agentworkspace once a second product wants it". The same path
fits here:

1. Build `dax host` step 3 with its registry in a dax package of its
   own, held by an import-boundary test to agentturn, agentsession,
   agentworkspace's root and the standard library, with no dax import.
2. Port dexclaw onto a copy of that package.
3. Extract it as a sibling if it passes the test in "The case against".

**Placement once extracted**, two candidates:

- A new sibling module. It depends on `agentturn` (`Control`,
  `front/control`), `agentsession` (`Store`, `RefStore`, the ref
  helpers), `agentworkspace` (`Workspace`, `Descriptor`; its root is
  standard library only) and `openresponses`. It does not depend on
  agentkit (the `kitsession` adapter is a nested module) or on
  `front/a2a`, which a product mounts through `Mounter`. dax and dexclaw
  import it, and nothing below it does.
- A nested module of agentturn, `front/host`. It is a front of many
  `Control`s, beside `front/control` and `front/a2a`. Against it:
  agentturn's AGENTS.md keeps agentsession to its `session` module,
  and the host needs refs and agentworkspace, which agentturn has never
  imported.

**Names** (not picked):

- `agenthost`: the role, in the workspace's `agent*` form. Both
  products already call the layer "host".
- `agentgateway`: OpenClaw's word for the process. It also suggests
  channel adapters, which this would not have.
- `agentturn/front/host`: if it lands as agentturn's nested module.

**dexclaw** builds a host over its CAS store with `WithRefPrefix`,
`WithIdle(*idle)` and `WithAutoOpen()`. Its factory makes the kit
(`newKit`, unchanged), reaps what `inspect` finds, makes the agent,
subscribes the deliverer, and returns `Kit.Control` wrapped in its
unattended `Prompt`. `internal/api` and `internal/clock` call
`Dispatch`. It keeps: the kit options, the reviewer, the reaper,
delivery and tickets, the budget, the clock, the migration gate.

```go
h, err := agenthost.New(store, func(ctx context.Context, o agenthost.Opening) (agenthost.Session, error) {
	open := agentkit.WithResumedSession(store, o.SessionID)
	if o.SessionID == "" {
		open = agentkit.WithSession(store, header)
	}
	k, err := newKit(ctx, string(o.Key), open)
	if err != nil {
		return nil, err
	}
	reap(o.Key, k.Session())
	a := agentturn.New(k.Config(), k.AgentOptions()...)
	unsub := []func(){k.Attach(a), deliverer.Attach(string(o.Key), a)}
	ctl, err := k.Control(a)
	if err != nil {
		return nil, errors.Join(err, k.Close())
	}
	return unattended(k, ctl, reviewer, unsub), nil
}, agenthost.WithRefPrefix("dexclaw/conversations"), agenthost.WithIdle(*idle), agenthost.WithAutoOpen())
```

**dax** builds a host with `WithMaxLive`, `WithIdle` and a resolver
(a directory under `dirs`, or `hub.Workspace(ctx, name)` with the
tunnel's replaced signal as `Lost`). Its factory is step 2's factored
session build with the request as a layer. It returns `*agent.Session`,
which is a `Session` already, and gives it `Commands` (serve's) and
`Mounts` (its A2A executor and card). It mounts `h.Handler(...)` under
`/host` with `internal/authn`'s authenticator and challenge, beside
`/tunnel` and `/store/`. It keeps: the host config, the tokens, the
open request's rules, the hub and its enrollment, the workspace
re-check, the A2A store, and `dax serve` as the one-conversation mode.

## The case against

- **The factory may be the product.** Everything that took judgment in
  either host is on the product's side of the line: who answers a held
  call, what to reap, which workspace a session may have and when a
  replacement is the same one, what an open request may set, who may
  ask. What is left is a map behind a mutex, a ticker, a starting flag
  and a mux. Counted on dexclaw, that is about 195 logic lines. The
  share both products use (registry, idle, reopen by ref, holds,
  shutdown, status) is nearer 150, since dax opens explicitly and
  drives its sessions through `front/control`, not `Dispatch`. A module
  of that size has its own release train, CI and boundary, and four of
  the six sibling issues above would be filed partly to make it clean.
- **The lifecycles may not be one lifecycle.** dexclaw opens on any
  input, has no views, and idles on "not running". dax opens on
  request, idles only with no view, no run and no A2A task, and must
  re-check a workspace that moved. If `WithAutoOpen`, `Dispatch`,
  `Busy`, `Mounter` and `WithOnLost` each have one user, the package is
  two products' code sharing a struct.
- **The siblings may absorb the rest.** With refs (already shipped),
  `Deliver` on `Control`, a named `Releaser` and agentkit's unattended
  control, a product's own registry might be under 100 lines. That fits
  the precedent of agentkit's `examples/a2a` `RecordEach`: a documented
  pattern, "copy it", not a module.

**The test.** Build `dax host`'s registry inside dax behind an import
boundary, then port dexclaw onto a copy without editing the copied
code beyond adding options. It is a library if all of these hold:

1. The `Factory` and `Session` signatures did not change for dexclaw.
2. The port needed at most one option that only dexclaw uses.
3. At least about 150 lines of the package are used by both.
4. dexclaw's `host.go` shrinks by more than half beyond what refs and a
   current agentkit remove on their own.

If any fails, it is a pattern: write it up in agentkit's
`docs/composition.md` next to `RecordEach`, file the sibling gaps
anyway (each stands alone), and let each product keep its registry.

## Open questions

- Route by key or by session ID? A key survives a rollover (`ContinueRef`
  moves the ref) where an ID does not. PR #135 routes by ID.
- Should `Dispatch` and its modes be in the library, or only `Control`
  plus `Deliver` once agentturn has it?
- A typed open request (`Host[R any]`, the host decoding `R`) or
  `json.RawMessage` as above?
- Should the A2A mount be the library's (a nested module over
  `front/a2a`, `RecordEach` built in) or stay the product's `Mounter`?
- Should the library ship a client of `/sessions` for `dax host
  ls|open|close`?
- Does the record of the open request belong in the header's extra
  members (the product's, as above), or in an entry the library defines?
- Where to incubate: dax (the first real consumer) or agentstudies (the
  longest-measured one)?
