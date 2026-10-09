# A control contract for the human plane, with questions as events, and a native front that serves it

Status: optional. The sandbox case does not need it (execution-boundary.md); this is for running the human or autonomous plane apart from the turn.

Repositories: agentturn (the contract, questions as events, `front/control`),
agentkit (a helper that wraps a kit's agent as the contract). Draft for
filing, agentturn first; agentkit's part follows its release.

## The problem

A turn joins three planes: inference (`openresponses.Streamer`), execution
(the tools, over a workspace), and the human or autonomous plane that
prompts, steers, and answers what the policy asks. agentturn owns the
first two contracts already (`Model`, the tool contract through
agenttool) and serves the loop over two protocols, Open Responses and A2A
(`docs/plans/agent-layer.md`, "Both directions for every protocol"). The
third plane has no contract of its own here:

- **The driving methods exist but are not named as a contract.**
  `*Agent` has them (`agent.go`: `Prompt` :404, `Resume` :647, `Steer`
  :1067, `FollowUp` :1076, `Abort` :1365, `State` :370, `Subscribe`
  :1004), so each front and each client restates them: agentconsole
  defines its own `client.Control` and `client.LiveEvent`
  (`agentconsole/client/client.go:28-96`, `client/live.go`), a narrowed copy
  of agentturn's events that has already lost fields (below).
- **A question asked mid-call is a callback.** `Config.ToolElicitor` is
  an `agenttool.Elicitor` the loop calls with the call on the context
  (`config.go:191-204`), and `Ask` puts a deferred nested call to it
  (`ask.go:62`). A callback cannot cross a wire, so agentconsole built
  its own question events and a reply (`client.QuestionAsked`, `Reply`,
  `native.Backend.Ask`, `agentconsole/client/native/native.go:524`) on top.
- **Nothing serves the plane over a network.** A product that runs its
  agent where the code is and drives it from elsewhere has no wire for
  it; A2A is the peer protocol, lossy by design (tasks and artifacts, not
  the transcript), and Open Responses serves the loop as a model.

dax has hit each of these: it hosts the human plane in its session,
drives every front through agentconsole's `client.Backend`, and found the
narrowed events missing what it needed.

## The proposal

### 1. `Control`, in the root module

```go
// Control is how the human or autonomous plane drives an agent: a
// person at a client, or a controller that answers by rule. *Agent is
// its in-process implementation.
type Control interface {
	Prompt(ctx context.Context, items ...openresponses.Item) (*RunEnd, error)
	Resume(ctx context.Context, answers ...Answer) (*RunEnd, error)
	Steer(items ...openresponses.Item)
	FollowUp(items ...openresponses.Item)
	Abort()
	State() State
	Subscribe(fn func(context.Context, Event) error) (unsubscribe func())
	Reply(id string, r Reply) error
}
```

Every method but `Reply` exists on `*Agent` with this signature, so the
contract names what is there. The events a subscriber gets are
agentturn's own (`events.go`), unnarrowed: what a client needs that the
loop knows is already on them.

### 2. Questions as events, with a reply

A question asked while a call runs (a tool's elicitation, a nested call
the hook deferred, a sub-agent's call its policy asked about) becomes an
event and an answer, so it can cross a wire like everything else:

```go
type Question struct{ ID string; Call *AskedCall; Elicitation agenttool.Elicitation }
func (*Question) EventType() string // "question"
type QuestionClosed struct{ ID string; Reply *Reply } // "question_closed"; Reply nil when the call gave up
type Reply struct{ Accept bool; Note string; Content json.RawMessage }

// QuestionElicitor is an agenttool.Elicitor that asks through the
// agent's subscribers: it publishes Question, blocks until Reply with
// its ID or the call's context ends, and publishes QuestionClosed.
func (a *Agent) QuestionElicitor() agenttool.Elicitor
```

A host sets `Config.ToolElicitor = agent.QuestionElicitor()` (or the loop
installs it when `ToolElicitor` is nil and an option asks for it). A
subscriber that attaches while a question waits is sent it, since it still
wants an answer; no other past event is replayed. The record of what the
user decided stays the asker's, as now (the session recorder's
`Elicitor` wrapper, `config.go:197-199`). `ToolElicitor` as a callback
stays for hosts that answer in process.

### 3. `front/control`: the native front and its client

A nested module beside `front/a2a` and `front/responses`, with both
directions, as the plan's rule asks:

```go
func Handler(c agentturn.Control, opts ...Option) http.Handler   // serve
func Dial(ctx context.Context, url string, opts ...DialOption) (*Client, error) // consume; *Client implements agentturn.Control
```

- **Transport.** HTTP: JSON requests for the methods; `Subscribe` as a
  server-sent event stream of agentturn events in a JSON envelope
  (`{"type": "<EventType>", ...}`). `Prompt` and `Resume` complete when
  the run ends, returning `RunEnd`. openresponses' `sse` package is at
  hand. Fields that are not data (`PendingCall.Tool`, an `error`) travel
  as their description (the tool's name and annotations; the error's
  text).
- **What is committed comes from the record, not the stream.** The
  stream carries only what is not committed yet, and a reconnect is a new
  subscription that replays nothing but a waiting question. A client
  catches up on everything committed by following the session from its
  agentsession cursor, over agentsession's store protocol
  (`agentsession/docs/rfcs/0003-agent-session-store-protocol.md:341-408`:
  `snapshot`, `appended`, `head`, `reset`, `committed`, SSE ids as
  cursors, `Last-Event-ID` to resume, `reset` rather than a silent skip).
  `front/control` does not carry the record itself; a host that serves
  both mounts the store's handler beside it. This keeps agentsession out
  of the root module, which imports only openresponses and agenttool
  (`agentturn/AGENTS.md`, Module).
- **Authentication and TLS**, modelled on RFC 0003's security section
  (`:788-816`): every request authenticated over TLS, with scopes the
  handler checks: `read` (`Subscribe`, `State`), `control` (`Prompt`,
  `Steer`, `FollowUp`, `Abort`), `answer` (`Resume`, `Reply`: what lets a
  call run that the policy asked about, so never implied by `read`), and
  `command` (below). The authenticator is an option; the handler puts the
  principal on each run's trigger so the record says who answered.

### 4. Product controls: on the front, not in the contract

A product has controls the loop does not: dax switches model and
reasoning, adds and removes MCP servers. They do not belong in
`agentturn.Control`, whose methods are the loop's own vocabulary. A model
or reasoning switch is `Agent.SetConfig` underneath, but `Config` holds
tools and hooks and is not data, so `SetConfig` cannot cross a wire whole.

Proposed: `front/control` carries named commands. `Handler(c,
WithCommands(map[string]Command))`, where `Command` is `func(ctx,
json.RawMessage) (json.RawMessage, error)`, served under the `command`
scope; `Client.Command(ctx, name, args)` and `Client.Commands(ctx)`
(name, description, argument schema) on the client. In process a product
calls its own functions; over the wire the same functions are the
registered commands. The contract stays the loop's.

## agentkit: a kit's agent as the contract

A kit's agent cannot be driven as a bare `*Agent`: an answer must go
through the kit's engine first, so a call the engine only held for a
sibling's ask is released with it (`agentpolicy.Engine.Release`), and a
run must carry the kit's recorder and session ID on its context. Today
agentconsole's `kitbackend` does both (`client/kitbackend/kitbackend.go:143-147`
wraps `Answer` in `eng.Release`; `runContext` at `:158` adds the
recorder and session ID; `WithHeadMove` revokes and re-grants skill
grants on a head move). That is the kit's composition, not the view's,
and belongs in agentkit:

```go
// Control is the agent built over k (k.Config, k.AgentOptions, k.Attach)
// as an agentturn.Control: Resume puts the answers through the kit's
// engine, and every run carries the kit's recorder and session.
func (k *Kit) Control(a *agentturn.Agent) agentturn.Control
```

A head move (agentconsole's `ContinueFrom`) is a recorder operation plus
the kit's skill-grant bookkeeping; the helper can offer it as an extra
method (`ContinueFrom(ctx, entryID)`) on the value it returns, outside
the contract, until a second consumer says it belongs in it. Measured
against agentkit's rule (`agentkit/AGENTS.md`, "The rule"), the helper is
writable with exported calls alone: it is the manual path, named.

## What it must not do

- Narrow the events: a subscriber over the wire gets what one in process
  gets, in a JSON form.
- Replay past live events on reconnect, other than a waiting question;
  the record is how a client catches up.
- Carry a product's vocabulary in `agentturn.Control`.
- Accept a request it has not authenticated, or grant `answer` with
  `read`.

## Tests

- `*Agent` satisfies `Control` (a compile-time assertion), and so do
  `control.Dial(control.Handler(agent))` over `httptest` and
  `kit.Control(agent)`.
- The loop's own driving tests (steer, follow-up, resume after a deferral,
  abort mid-batch) run against the dialled client unchanged.
- A tool's elicitation and a deferred nested call, under
  `QuestionElicitor`, reach a remote subscriber as `Question` and are
  answered by `Reply`; a subscriber attaching mid-question is sent it;
  an abort closes it with a nil reply.
- A token with `read` alone cannot `Resume`, `Reply` or `Prompt`.
- Through `kit.Control`, a deferred call and the sibling the engine held
  are both released by one answer (the case kitbackend's test covers
  today).

## What dax does with it

dax's session exposes `kit.Control(agent)` as its human plane. Today it
is dax's interim `agent.Turn` (`dax/agent/plane.go`), the shape this
proposal asks agentturn to own: Prompt, Answer (Resume after the
engine's release), Permissions, Steer, FollowUp, Abort, State,
Subscribe, and Questions with Reply. dax's own controls (model, think,
MCP, session info) are `agent.Controls` beside it, which become the
commands below.
Every front drives that: the terminal client, the REPL, and `-p` as a
controller that answers by rule. `dax serve` mounts `front/control` with
dax's commands (model, think, MCP, and session info) and the store's
follow handler beside it; `dax attach <url>` runs the same fronts over
`control.Dial`.

## Open questions

- Should the loop install `QuestionElicitor` itself when `ToolElicitor`
  is nil, or only on an option, since a run inside an MCP tool relies on
  the elicitor already on its context (`config.go:199-203`)?
- `Queue(ctx, mode, items...)` (`agent.go:1089`) generalizes `Steer` and
  `FollowUp`; should the contract carry it instead of the two?
- One handler per agent, or a handler over many with an ID in the path,
  which a session host that starts sessions remotely needs?
