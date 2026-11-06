package agent

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// refusing is a model whose every request the key's credit cannot
// cover.
type refusing struct{ calls atomic.Int32 }

func (m *refusing) CreateStream(context.Context, openresponses.Request, openresponses.EventSink) error {
	m.calls.Add(1)
	return &openresponses.Error{StatusCode: 402, Type: openresponses.ErrorTypeInvalidRequest, Code: openresponses.CodeInsufficientQuota, Message: "no credit"}
}

// A request the key's credit cannot cover is not sent again: the run
// ends on the first refusal, which is the record's (dax#15, agentturn
// v0.0.39's default retry).
func TestAQuotaRefusalIsNotRetried(t *testing.T) {
	ctx := context.Background()
	m := &refusing{}
	s, err := New(ctx, options(t, m))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	end, err := s.Prompt(ctx, openresponses.UserText("hello"))
	if err == nil && end != nil {
		err = end.Err
	}
	if !openresponses.IsInsufficientQuota(err) || end != nil && end.Reason != agentturn.ReasonError {
		t.Errorf("the run ended %+v: %v", end, err)
	}
	if n := m.calls.Load(); n != 1 {
		t.Errorf("%d model calls, want 1", n)
	}
}
