# agentconsole: narrow to the view of the record, over agentturn's control contract

Repository: agentconsole. Draft for filing after agentturn's control
contract (agentturn-control.md) ships; it depends on it.

## The problem

agentconsole's rule is that the record is the truth for what is
committed, and live events carry only what is not (`AGENTS.md`, top, and
Rules). Its job is a view: the reconciler (`view`), the per-tool
renderers (`toolview`), the TUI and the record panes. But its contract is
named `client.Backend` (`client/client.go:28`), with `Control`
(`:47-76`) and `Live` (`:36`), and it ships the two in-process backends
(`client/native`, `client/kitbackend`). The name and the packages say
agentconsole owns how an agent runs: it builds the agent from a kit,
attaches the recorder, releases held calls through the engine, and hosts
the questions a running call asks (`native.Backend.Ask`,
`client/native/native.go:524`). Those are the human plane's and the kit's
concerns, which agentturn and agentkit now carry (agentturn-control.md).

Re-stating agentturn's control and events here has also cost fidelity.
`client.LiveEvent` is a narrowed copy of agentturn's events
(`client/live.go`), and dax, driving every front through it, found each
of these missing:

| Gap in `client` | What agentturn already has |
|---|---|
| `Pending` has no held flag: a call that may have run before it was held cannot be told from one that never ran (`live.go`, `Pending{CallID, Name, Args, Reason}`) | `PendingCall.Dispatched`, `IdempotencyKey`, `Tool` (`agentturn/events.go:622-650`) |
| `RunEnded` has no cause: a stop for max turns, a guard, a terminating tool all read as "stopped" (`live.go`, `RunEnded{RunID, Reason, Err, Withheld, Pending}`) | `RunEnd.Cause` (`StopCause`) (`events.go:512-534`) |
| `ResponseCompleted` has no usage, so a client prices a call only from the record (`live.go`, `ResponseCompleted{RunID, ResponseID, Model, Status, Withheld}`) | `ResponseEnd.Response`, with its usage (`events.go:329-357`) |
| `TurnStarted` has no inputs: what arrived for the turn (steered items, tool outputs) is not on the event (`live.go`, `TurnStarted{RunID, Turn, Model}`) | `TurnStart.Inputs`, `Arrived`, `Request` (`events.go:177-183`) |
| `Control` has no `FollowUp` | `Agent.FollowUp` (`agentturn/agent.go:1076`) |

## The proposal

agentconsole consumes, and owns nothing that runs:

- **The view's inputs are agentturn's control contract and events, and
  an agentsession follower.** `console.Run(ctx, c agentturn.Control,
  rec Record, ...Option)`, where `Record` stays (`client/client.go:78-96`:
  `Follow`, `Verified`, `Read`, `Refs`), being the view's own reading of
  the record. Live events are agentturn's, as `Control.Subscribe`
  delivers them, copied in the subscriber as the Rules already require.
  `view` reconciles agentturn events with record `Change`s.
- **`client.Backend` becomes at most a pairing** of a `Control` and a
  `Record`, kept as a convenience for embedders, with no behaviour of its
  own; `Live` goes, since `Control.Subscribe` is it.
- **`client/native` and `client/kitbackend` leave.** What they do is the
  kit's and the loop's: the engine's `Release` on an answer, the run
  context, the skill grants on a head move, all move to agentkit's
  `Kit.Control` (agentturn-control.md, "agentkit"); questions become
  agentturn's `Question` events and `Reply`. What is left is glue: a
  `Record` over a `session.Recorder`'s store (`Follow` on the same store
  value, as backend 1 does today, `docs/plans/client.md:111-115`), which
  can stay here as `client/record` or move to agentturn's `session`
  module.
- **Over a wire nothing changes for the view:** `control.Dial` is a
  `Control`, and an agentsession store client (RFC 0003) is a `Record`.
  Backend 3 of the plan, "native over a wire, later"
  (`docs/plans/client.md:129-136`), is those two, owned where the plane
  and the record are.

### What disappears

Every row of the table above disappears: the view reads agentturn's
events, which carry the held flag, the cause, the usage and the inputs,
and calls `Control.FollowUp`. `client.Question`/`Reply` and
`QuestionAsked`/`QuestionClosed` are replaced by agentturn's. The plan's
"Decided in implementation, step 6 (questions)" (`docs/plans/client.md:581-618`)
moves its decisions to agentturn with them: a question is asked while the
run goes, is sent to a subscriber that attaches while it waits, is not
recorded by the view.

What does not disappear: the ACP client (backend 2, `client.md:116-128`)
synthesizes a record from a foreign agent and needs a `Control`
implementation that is not an `*Agent`; it is the first test that the
contract is not shaped by agentturn's internals.

## What it must not do

- Show a committed fact from a live event: the reconciler's rules stand
  (`AGENTS.md`, Rules).
- Keep a second copy of agentturn's event types; a field the view needs
  that the loop lacks is a request to agentturn.
- Import agentkit outside an example or a test.

## Tests

- The TUI's scripted-model tests run over `*agentturn.Agent` directly,
  over `kit.Control`, and over `control.Dial` in `httptest`, unchanged.
- `view` tests drive agentturn events and record changes; the cases for
  each table row above (a held call shown as "may have run", a stop
  cause, a priced response before its entry lands, a turn's inputs) pass.
- A question asked by a sub-agent's policy is shown and answered with
  `Reply` over each control.

## What dax does with it

dax drops its glue (`dax/tuiadapter.go` today adapts the session's
`agent.Turn` and record to `client.Backend` over `client/native`, and
lists at its top what it has to fake) and hands the terminal client its
session's `kit.Control(agent)` and a record over its store. `dax attach`
hands it `control.Dial` and a store client.

## Open questions

- Does the repository keep its name once it is the view alone? (The plan
  already lists the name as a working one, `client.md:1031-1033`.)
- Should `Record` move to agentsession, beside `Follower`, so a view and
  a store client share it?
