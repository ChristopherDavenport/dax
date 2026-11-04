package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/export"
)

// WriteTrajectory writes the session as an ATIF document to path
// (agentsession's export, RFC 0001's ATIF projection): the path to its
// head as the store holds it now, each sub-agent's session embedded,
// priced by price when it is not nil. The sessions are read without a
// hold, so it works beside the session still open.
func (s *Session) WriteTrajectory(ctx context.Context, path string, price Price) error {
	r, ok := s.store.(agentsession.Reader)
	if !ok {
		return errors.New("trajectory: the session is not recorded in a store that can be read")
	}
	rec, err := r.Read(ctx, s.ID())
	if err != nil {
		return fmt.Errorf("trajectory: %w", err)
	}
	t, err := export.At(rec, rec.Leaf())
	if err != nil {
		return fmt.Errorf("trajectory: %w", err)
	}
	opts := export.Options{
		AgentName: s.opts.Name, AgentVersion: s.opts.Version,
		Subsessions: func(id string) (*agentsession.Session, error) { return r.Read(ctx, id) },
	}
	if price != nil {
		opts.Cost = price
	}
	doc, err := export.ToATIF(t, opts)
	if err != nil {
		return fmt.Errorf("trajectory: %w", err)
	}
	if err := export.WriteDocument(path, doc); err != nil {
		return fmt.Errorf("trajectory: %w", err)
	}
	return nil
}
