# A2A peers: link a peer call in the record, cancel on abort, resume input-required, authenticate the server

Repositories: agentturn (`tools/a2a`, `front/a2a`), with one format
question for agentsession. Draft for filing; the agentsession part is an
RFC 0001 change and goes there first.

## The problem

The plan gives A2A the peer role: `front/a2a` exposes a loop to A2A
callers and `tools/a2a` wraps a remote A2A agent as a `Tool`
(`docs/plans/agent-layer.md`, "As a peer", and the both-directions table
just below it). Compared with the in-process peer, `tools/agent`, the A2A
pair leaves four things to the host, and a product that composes two
agent systems needs each of them:

1. **The record does not say a peer did the work.** A `tools/agent`
   child is a session of its own, linked from the parent by a `link`
   entry with `rel: subsession` and the call ID, written at dispatch
   (`session/session.go:247-253`, agentsession `RelSubsession`,
   `agentsession/entry.go:38-44`). An A2A call leaves only
   `TaskInfo{TaskID, ContextID, State}` in `Result.Details`
   (`tools/a2a/a2a.go:37-43`): a reader of the caller's session cannot
   find where the answer came from, and `-verify` and a tree view cannot
   point at the peer.
2. **An abort does not reach the peer.** Cancelling the caller's context
   abandons the request; the remote task keeps running
   (`tools/a2a/a2a.go`, the streaming and blocking paths). A user who
   aborts expects the work to stop.
3. **input-required ends the call and nothing resumes it.**
   `InputRequiredError` (matching `agentturn.ErrInputRequired`) carries
   the remote agent's question to the model (`tools/a2a/a2a.go:45-65`,
   `:285`); continuing the same task with an answer is left to the host,
   and there is no hook to do it.
4. **`front/a2a` has no authentication.** The agent card declares no
   security schemes (`front/a2a/card.go:18-49`) and nothing checks a
   caller. A served agent runs tools; serving it unauthenticated is not an
   option outside a test.

## The proposal

### 1. A peer link (agentsession format, then `session`)

A new relation, `peer`, on `LinkEntry`: written in the caller's session
at dispatch, with the call ID, naming the peer's task and context and the
agent card URL as members (the peer's session is not in the caller's
store, so `Session` is empty or the peer's own ID if its card publishes
one). `tools/a2a` reports the task through the call's record
(`agenttool.WriteRecord`) as soon as the task exists; `session`'s
recorder writes the link from it, as it writes a subsession link from a
`tools/agent` ChildInfo. The RFC says what a reader may conclude from a
`peer` link: that the output came from that task, not that it can be
replayed.

### 2. Cancel on abort (`tools/a2a`)

When the call's context ends before the task does, `tools/a2a` sends
`tasks/cancel` for the task, with a short deadline of its own, and
returns the context's error. Opt out with `WithoutCancelOnAbort` for a
peer whose tasks should outlive a caller.

### 3. Resuming input-required (`tools/a2a`)

`WithInputRequired(func(ctx, TaskInfo, question) (answer []a2a.Part, err error))`:
when the peer asks, the hook answers on the same task and the call goes
on; with no hook, or when the hook returns `ErrInputRequired`, the call
ends as now. A product's hook is how the caller's human plane answers a
peer's question inside a call: dax wires it to the agent's
`QuestionElicitor` (agentturn-control.md), so the question reaches whoever
holds the session's `agentturn.Control`, as a
sub-agent's does.

### 4. Authentication (`front/a2a`)

`WithAuthenticator(func(*http.Request) (principal string, err error))`
on the handler, the matching security scheme on the card, and the
principal on each task's record as the run's trigger. TLS is the
deployment's. The model is agentsession RFC 0003's security section
(`agentsession/docs/rfcs/0003-agent-session-store-protocol.md:788-816`):
authenticate every request; record the principal beside what it caused.

### 5. Escalating the callee's asks (`front/a2a`, opt-in)

Today a call to one of the served agent's own tools that its policy asks
about goes to the server's `Config.ToolElicitor`, or is refused
(`front/a2a/doc.go:78-108`); only the caller's own tools come back as
input-required (`doc.go:55-76`). That stays the default: each system's
policy is its own, and its own human plane answers.

As an option the callee enables, `WithEscalation()`: a call its policy
defers ends the task input-required with the call and the policy's
reason as a question, and the caller's answer (allow, or refuse with a
note) releases or refuses it through the same `Answer` path a permission
uses. It is for a headless callee that nobody watches, whose caller's
human plane should decide. The record shows the decision `by` the caller,
with the authenticated principal.

## What it must not do

- Change `tools/a2a`'s result or error shapes for callers that set none
  of the options, beyond the link and the cancel.
- Escalate by default: a callee answers its own policy's asks unless it
  opted in.
- Trust the card: the link records the URL that was called, not the
  name the card claims.

## Tests

- A caller over `tools/a2a` against `front/a2a` in `httptest`: the
  caller's session has a `peer` link with the call ID, task and context;
  aborting the caller cancels the task (the server sees `tasks/cancel`,
  the run ends canceled); a peer that asks is answered by the
  `WithInputRequired` hook and completes in one call.
- `front/a2a` with an authenticator refuses an unauthenticated
  `message/send` and records the principal on an authenticated one.
- With `WithEscalation`, a callee whose policy asks ends input-required;
  the caller's allow runs the call and its refusal is recorded `by` the
  caller; without it, the server's elicitor answers as now.

## What dax does with it

An `a2a` extension (`ext/a2a`, a nested module for a2a-go's weight, as
agentkit keeps it out of its core: `agentkit/docs/composition.md:93-107`)
offers configured peers as tools, with its own rules (`extension:a2a`)
and renderers, and wires `WithInputRequired` to the agent's questions.
`dax serve` mounts `front/a2a` beside `front/control` (agentturn-control.md), with the
authenticator it uses for that wire, and offers escalation as a serve
flag for an unattended session host.

## Open questions

- Is `peer` one relation, or should a peer that publishes its session
  (a dax behind `front/a2a` with a store a caller can read under
  RFC 0003) be linked as `subsession` with a store URL?
- Does the cancel need to wait for the peer's `canceled` state before the
  call returns, so the record can say the work stopped?
