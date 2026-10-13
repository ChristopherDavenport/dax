package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
)

// A reply that came before the last view went stands; only a question
// given up for want of a view is nobody's to answer.
func TestAReplyBeforeTheLastViewWentStands(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errNoOne)
	accept := agenttool.Answer{Action: agenttool.ActionAccept}
	if ans, err := orNoOne(ctx)(accept, nil); err != nil || ans.Action != agenttool.ActionAccept {
		t.Errorf("a reply that won the race: %+v, %v; want it to stand", ans, err)
	}
	if _, err := orNoOne(ctx)(agenttool.Answer{}, ctx.Err()); !errors.Is(err, errNoOne) {
		t.Errorf("a question given up: %v, want nobody to answer", err)
	}
	if _, err := orNoOne(context.Background())(agenttool.Answer{}, context.Canceled); !errors.Is(err, context.Canceled) {
		t.Errorf("a question cut off otherwise: %v, want its own error", err)
	}
}
