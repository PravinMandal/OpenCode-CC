package server

import (
	"context"
	"testing"
	"time"

	"github.com/Kiowx/opencode-cc/internal/store"
)

// TestVacuumSchedulePersistsAcrossRestarts proves the VACUUM interval is
// anchored to the database, not process uptime. A service restarted every few
// days used to reset the weekly timer on every start and never vacuum.
func TestVacuumSchedulePersistsAcrossRestarts(t *testing.T) {
	path := t.TempDir() + "/test.db"
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	srv := &Server{store: st}
	now := time.Now()

	// First run records a baseline instead of vacuuming immediately.
	if got := srv.lastVacuumAt(now); !got.Equal(now) {
		t.Fatalf("fresh baseline = %v, want %v", got, now)
	}
	ctx := context.Background()
	if ms, ok := st.MetaInt(ctx, lastVacuumMetaKey); !ok || time.UnixMilli(ms).Sub(now) > time.Second {
		t.Fatalf("baseline not persisted: %d/%v", ms, ok)
	}

	// An overdue timestamp is read back from disk.
	overdue := now.Add(-2 * vacuumInterval)
	if err := st.SetMetaInt(ctx, lastVacuumMetaKey, overdue.UnixMilli()); err != nil {
		t.Fatalf("seed overdue: %v", err)
	}
	if got := srv.lastVacuumAt(now); got.UnixMilli() != overdue.UnixMilli() {
		t.Fatalf("persisted vacuum = %v, want %v", got, overdue)
	}

	// It runs when due and advances the persisted timestamp.
	if got := srv.vacuumIfDue(now, overdue); !got.Equal(now) {
		t.Fatalf("vacuumIfDue = %v, want %v", got, now)
	}
	if ms, ok := st.MetaInt(ctx, lastVacuumMetaKey); !ok || ms != now.UnixMilli() {
		t.Fatalf("vacuum time not persisted: %d/%v", ms, ok)
	}
	// It does not run again inside the interval.
	if got := srv.vacuumIfDue(now.Add(time.Hour), now); !got.Equal(now) {
		t.Fatalf("vacuum ran early: %v", got)
	}

	// A clock-skewed future timestamp must not stall the schedule.
	future := now.Add(48 * time.Hour)
	if err := st.SetMetaInt(ctx, lastVacuumMetaKey, future.UnixMilli()); err != nil {
		t.Fatalf("seed future: %v", err)
	}
	if got := srv.lastVacuumAt(now); !got.Equal(now) {
		t.Fatalf("future timestamp = %v, want clamped %v", got, now)
	}
	recent := now.Add(-time.Hour)
	if err := st.SetMetaInt(ctx, lastVacuumMetaKey, recent.UnixMilli()); err != nil {
		t.Fatalf("seed recent: %v", err)
	}
	_ = st.Close()

	st2, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = st2.Close() })
	srv2 := &Server{store: st2}
	if got := srv2.lastVacuumAt(now); got.UnixMilli() != recent.UnixMilli() {
		t.Fatalf("reopened store vacuum = %v, want %v", got, recent)
	}
}
