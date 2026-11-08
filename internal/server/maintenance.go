package server

import (
	"context"
	"log"
	"os"
	"strconv"
	"time"
)

// Request-log retention.
//
// The request log is a debugging aid, not a ledger: it is an append-only table
// that grew without bound while the panel kept scanning it. The hourly
// aggregates stay forever, so lifetime totals and charts are unaffected by a
// trim. Defaults are deliberately generous (two weeks, 200k rows) and can be
// overridden without touching config.json:
//
//	OPENCODE_CC_LOG_RETENTION_DAYS=0   keep every row by age
//	OPENCODE_CC_LOG_MAX_ROWS=0         keep every row by count
const (
	defaultRetentionDays = 14
	defaultMaxRows       = 200000
	maintenanceInterval  = 6 * time.Hour
	// VACUUM rewrites the whole file and blocks writers; weekly is often
	// enough to reclaim space freed by the trims in between.
	vacuumInterval   = 7 * 24 * time.Hour
	maintenanceLimit = 2 * time.Minute
	// lastVacuumMetaKey persists the last successful VACUUM in SQLite. A
	// process-uptime clock restarted the interval on every restart, so a
	// service that restarted weekly never vacuumed at all.
	lastVacuumMetaKey = "last_vacuum_ms"
)

func retentionDays() int {
	return envInt("OPENCODE_CC_LOG_RETENTION_DAYS", defaultRetentionDays)
}

func maxRequestRows() int {
	return envInt("OPENCODE_CC_LOG_MAX_ROWS", defaultMaxRows)
}

func envInt(name string, fallback int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return fallback
	}
	return n
}

// StartMaintenance launches the retention loop. It returns immediately and
// stops when Shutdown closes s.done.
func (s *Server) StartMaintenance() {
	if s == nil || s.store == nil {
		return
	}
	s.maintenanceOnce.Do(func() {
		go s.maintainLoop()
		log.Printf("opencode-cc: request-log retention every %s (keep %dd, %d rows max)",
			maintenanceInterval, retentionDays(), maxRequestRows())
	})
}

func (s *Server) maintainLoop() {
	now := time.Now()
	s.maintainOnce(now)
	lastVacuum := s.lastVacuumAt(now)
	lastVacuum = s.vacuumIfDue(now, lastVacuum)
	ticker := time.NewTicker(maintenanceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case now := <-ticker.C:
			s.maintainOnce(now)
			lastVacuum = s.vacuumIfDue(now, lastVacuum)
		}
	}
}

// lastVacuumAt returns the persisted time of the last successful VACUUM. A
// database that has never vacuumed records now, so a fresh install waits a
// full interval instead of rewriting the file on every restart.
func (s *Server) lastVacuumAt(now time.Time) time.Time {
	if s == nil || s.store == nil {
		return now
	}
	ctx, cancel := context.WithTimeout(context.Background(), maintenanceLimit)
	defer cancel()
	if ms, ok := s.store.MetaInt(ctx, lastVacuumMetaKey); ok {
		at := time.UnixMilli(ms)
		if at.After(now) {
			// Clock skew: never wait longer than a full interval.
			return now
		}
		return at
	}
	if err := s.store.SetMetaInt(ctx, lastVacuumMetaKey, now.UnixMilli()); err != nil {
		log.Printf("opencode-cc: record vacuum baseline: %v", err)
	}
	return now
}

// vacuumIfDue runs VACUUM when the persisted interval has elapsed and returns
// the timestamp the caller should keep in memory.
func (s *Server) vacuumIfDue(now, lastVacuum time.Time) time.Time {
	if s == nil || s.store == nil || now.Sub(lastVacuum) < vacuumInterval {
		return lastVacuum
	}
	ctx, cancel := context.WithTimeout(context.Background(), maintenanceLimit)
	defer cancel()
	if err := s.store.Vacuum(ctx); err != nil {
		log.Printf("opencode-cc: database vacuum: %v", err)
		return lastVacuum
	}
	// A fresh budget: VACUUM can consume the whole context above, and losing
	// the baseline write would make the next restart vacuum again at startup
	// (blocking writers for the full timeout).
	metaCtx, metaCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer metaCancel()
	if err := s.store.SetMetaInt(metaCtx, lastVacuumMetaKey, now.UnixMilli()); err != nil {
		log.Printf("opencode-cc: record vacuum time: %v", err)
	}
	return now
}

func (s *Server) maintainOnce(now time.Time) {
	if s == nil || s.store == nil {
		return
	}
	days := retentionDays()
	var cutoff time.Time
	if days > 0 {
		cutoff = now.AddDate(0, 0, -days)
	}
	ctx, cancel := context.WithTimeout(context.Background(), maintenanceLimit)
	defer cancel()
	if err := s.store.Maintain(ctx, cutoff, maxRequestRows()); err != nil {
		log.Printf("opencode-cc: request-log maintenance: %v", err)
	}
}
