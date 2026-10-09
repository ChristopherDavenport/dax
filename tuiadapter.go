package dax

// This file is the glue that shows a session in agentconsole's terminal
// client. agentconsole is a view of the record: it follows the session
// and drives the agent through its client.Backend. The session's human
// plane is its agent.Turn, so the glue presents the Turn as a Backend,
// over the session's one agent, with agentconsole's native backend
// mapping the agent's events to live events and following the store.
//
// What the glue has to fake or drop, which agentconsole's Backend would
// need to carry a Turn whole:
//
//   - Prompt and Answer return an error alone, so how a run ended (the
//     RunEnd, its Cause) reaches the client only as live events and the
//     record.
//   - A prompt while calls a stopped session held wait is refused with
//     agent.ErrHeld, which the client shows as an error; it shows the
//     held calls from the record and their answers go to Answer.
//   - The client has no follow-up, only Steer.
//   - Questions asked while a call runs are the Turn's; the glue asks
//     each through the native backend's own question list (Ask) and
//     carries the reply back, so a question lives in two lists.
//   - The native backend tracks the reasoning models of the branch a
//     head move went to and puts them on its own runs' contexts; the
//     glue's runs go through the Turn, which does not carry them, so a
//     run after a head move may send another model's reasoning items.
//   - The client cannot tell a call the policy asked about from one its
//     engine only holds beside it, and asks about both; Answer releases
//     either way.

import (
	"context"

	"github.com/ChristopherDavenport/agentconsole/client"
	"github.com/ChristopherDavenport/agentconsole/client/native"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/agent"
)

// consoleBackend is a session's Turn as agentconsole's client.Backend:
// the native backend over the session's agent, whose Live and Record
// the client reads, with its Control's runs going through the Turn.
type consoleBackend struct {
	*native.Backend
	t agent.Turn
	// stop ends the bridge from the Turn's questions to the native
	// backend's.
	stop func()
}

// newConsoleBackend builds the glue for sess.
func newConsoleBackend(sess *agent.Session) (*consoleBackend, error) {
	kit := sess.Kit
	opts := []native.Option{
		native.WithRunContext(sess.RunContext),
		native.WithHeadMove(
			func(ctx context.Context) error {
				kit.RevokeSkillGrants(ctx)
				return nil
			},
			func(ctx context.Context, s *agentsession.Session) error { return kit.RegrantSkills(ctx, s) },
		),
	}
	if eng := kit.Engine(); eng != nil {
		opts = append(opts, native.WithRelease(func(ctx context.Context, end *agentturn.RunEnd, answers []agentturn.Answer) ([]agentturn.Answer, error) {
			return eng.Release(ctx, end, answers...)
		}))
	}
	nb, err := native.New(sess.Agent(), kit.Recorder(), opts...)
	if err != nil {
		return nil, err
	}
	t := sess.Turn()
	// Each question the Turn asks is asked in the client too, and the
	// client's reply goes back to the Turn; a question the call gave up
	// is closed in the client.
	stop := t.Questions(func(q agent.Question) {
		go func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				select {
				case <-q.Done:
					cancel()
				case <-ctx.Done():
				}
			}()
			r, err := nb.Ask(ctx, client.Question{Call: q.Call, Text: q.Text})
			if err == nil {
				t.Reply(q.ID, agent.Reply{Accept: r.Accept, Note: r.Note})
			}
		}()
	})
	return &consoleBackend{Backend: nb, t: t, stop: stop}, nil
}

// Control is the native backend's, with the runs going through the Turn.
func (b *consoleBackend) Control() client.Control {
	return consoleControl{Control: b.Backend.Control(), t: b.t}
}

// Close stops the bridge of questions.
func (b *consoleBackend) Close() error {
	b.stop()
	return nil
}

type consoleControl struct {
	client.Control
	t agent.Turn
}

func (c consoleControl) Prompt(ctx context.Context, items ...openresponses.Item) error {
	_, err := c.t.Prompt(ctx, items...)
	return err
}

func (c consoleControl) Answer(ctx context.Context, answers ...agentturn.Answer) error {
	_, err := c.t.Answer(ctx, answers...)
	return err
}

func (c consoleControl) Steer(ctx context.Context, items ...openresponses.Item) error {
	return c.t.Steer(ctx, items...)
}

func (c consoleControl) Abort() { c.t.Abort() }

func (c consoleControl) State() agentturn.State { return c.t.State() }
