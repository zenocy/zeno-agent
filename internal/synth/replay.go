package synth

import (
	"context"
	"time"

	"github.com/zenocy/zeno-v2/internal/log"
)

// SliceReader wraps a log.Reader and intersects every read with [-, Until]
// so projections compute as if the log had been truncated at Until. Used by
// the replay CLI to re-run synth against a historical state of the log.
type SliceReader struct {
	Inner log.Reader
	Until time.Time
}

// Since proxies to Inner.Since, then drops events past Until.
func (s *SliceReader) Since(ctx context.Context, t time.Time) ([]log.Event, error) {
	out, err := s.Inner.Since(ctx, t)
	if err != nil {
		return nil, err
	}
	return s.clamp(out), nil
}

// ByKind proxies to Inner.ByKind, then drops events past Until.
func (s *SliceReader) ByKind(ctx context.Context, kinds ...string) ([]log.Event, error) {
	out, err := s.Inner.ByKind(ctx, kinds...)
	if err != nil {
		return nil, err
	}
	return s.clamp(out), nil
}

// Latest returns the most recent event of kind that is not past Until.
func (s *SliceReader) Latest(ctx context.Context, kind string) (*log.Event, error) {
	events, err := s.Inner.ByKind(ctx, kind)
	if err != nil {
		return nil, err
	}
	clamped := s.clamp(events)
	if len(clamped) == 0 {
		return nil, nil
	}
	last := clamped[len(clamped)-1]
	return &last, nil
}

func (s *SliceReader) clamp(events []log.Event) []log.Event {
	if s.Until.IsZero() {
		return events
	}
	out := make([]log.Event, 0, len(events))
	for _, e := range events {
		if e.TS.After(s.Until) {
			continue
		}
		out = append(out, e)
	}
	return out
}
