package clock

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stubSource is a minimal LocationSource that returns whatever Location is
// stored in its atomic pointer. Lets us simulate live settings.Reload() swaps
// without depending on the settings package.
type stubSource struct {
	loc atomic.Pointer[time.Location]
}

func newStubSource(loc *time.Location) *stubSource {
	s := &stubSource{}
	s.loc.Store(loc)
	return s
}

func (s *stubSource) TZ() *time.Location { return s.loc.Load() }

func (s *stubSource) set(loc *time.Location) { s.loc.Store(loc) }

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

func TestReal_LiveLocationFollowsSource(t *testing.T) {
	la := mustLoad(t, "America/Los_Angeles")
	athens := mustLoad(t, "Europe/Athens")

	src := newStubSource(la)
	c := NewReal(src)

	if got := c.Location(); got.String() != la.String() {
		t.Fatalf("initial: want %s got %s", la, got)
	}
	if got := c.Now().Location().String(); got != la.String() {
		t.Fatalf("Now() zone: want %s got %s", la, got)
	}

	src.set(athens)
	if got := c.Location(); got.String() != athens.String() {
		t.Fatalf("after swap: want %s got %s", athens, got)
	}
	if got := c.Now().Location().String(); got != athens.String() {
		t.Fatalf("Now() after swap: want %s got %s", athens, got)
	}
}

func TestReal_NilSourceFallsBackToUTC(t *testing.T) {
	c := NewReal(nil)
	if got := c.Location(); got != time.UTC {
		t.Fatalf("nil source: want UTC got %s", got)
	}
	if got := c.Now().Location(); got != time.UTC {
		t.Fatalf("nil source Now() zone: want UTC got %s", got)
	}
}

func TestReal_NilLocationFallsBackToUTC(t *testing.T) {
	src := newStubSource(nil)
	c := NewReal(src)
	if got := c.Location(); got != time.UTC {
		t.Fatalf("nil TZ: want UTC got %s", got)
	}
}

func TestFixed_NowAndLocation(t *testing.T) {
	la := mustLoad(t, "America/Los_Angeles")
	t0 := time.Date(2026, 5, 7, 9, 0, 0, 0, la)
	c := NewFixed(t0, la)

	if got := c.Now(); !got.Equal(t0) {
		t.Fatalf("Now: want %s got %s", t0, got)
	}
	if got := c.Location(); got != la {
		t.Fatalf("Location: want %s got %s", la, got)
	}
}

func TestFixed_NilLocationFallsBackToUTC(t *testing.T) {
	t0 := time.Date(2026, 5, 7, 9, 0, 0, 0, time.UTC)
	c := NewFixed(t0, nil)
	if got := c.Location(); got != time.UTC {
		t.Fatalf("Location: want UTC got %s", got)
	}
}

func TestFixed_AdvanceMovesForwardAndBackward(t *testing.T) {
	t0 := time.Date(2026, 5, 7, 9, 0, 0, 0, time.UTC)
	c := NewFixed(t0, time.UTC)

	c.Advance(time.Hour)
	if got := c.Now(); !got.Equal(t0.Add(time.Hour)) {
		t.Fatalf("after +1h: want %s got %s", t0.Add(time.Hour), got)
	}
	c.Advance(-30 * time.Minute)
	if got := c.Now(); !got.Equal(t0.Add(30 * time.Minute)) {
		t.Fatalf("after -30m: want %s got %s", t0.Add(30*time.Minute), got)
	}
}

func TestFixed_SetLocationPreservesInstant(t *testing.T) {
	la := mustLoad(t, "America/Los_Angeles")
	athens := mustLoad(t, "Europe/Athens")

	t0 := time.Date(2026, 5, 7, 9, 0, 0, 0, la) // an instant
	c := NewFixed(t0, la)

	c.SetLocation(athens)
	got := c.Now()
	if !got.Equal(t0) {
		t.Fatalf("instant should be preserved: want %s got %s", t0, got)
	}
	if got.Location().String() != athens.String() {
		t.Fatalf("zone: want %s got %s", athens, got.Location())
	}
}

func TestFixed_SetReplacesTime(t *testing.T) {
	la := mustLoad(t, "America/Los_Angeles")
	c := NewFixed(time.Date(2026, 5, 7, 9, 0, 0, 0, la), la)

	t1 := time.Date(2026, 11, 1, 1, 30, 0, 0, la) // ambiguous fall-back
	c.Set(t1)
	if got := c.Now(); !got.Equal(t1) {
		t.Fatalf("after Set: want %s got %s", t1, got)
	}
}

func TestFixed_ConcurrentSafe(t *testing.T) {
	// Run with -race to verify no data race.
	la := mustLoad(t, "America/Los_Angeles")
	athens := mustLoad(t, "Europe/Athens")
	c := NewFixed(time.Date(2026, 5, 7, 9, 0, 0, 0, la), la)

	var readers sync.WaitGroup
	var writer sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = c.Now()
					_ = c.Location()
				}
			}
		}()
	}
	writer.Add(1)
	go func() {
		defer writer.Done()
		for i := 0; i < 200; i++ {
			if i%2 == 0 {
				c.SetLocation(athens)
			} else {
				c.SetLocation(la)
			}
			c.Advance(time.Minute)
		}
	}()
	writer.Wait()
	close(stop)
	readers.Wait()
}
