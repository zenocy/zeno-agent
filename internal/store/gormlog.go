// Package store contains shared SQLite/GORM helpers used across the
// observation log and the typed read-side stores.
package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// SlowQueryObserver receives one call per slow query. Mirror of
// metrics.ObserveDBQuery, kept as a callback so the store package stays free
// of an internal/metrics import.
type SlowQueryObserver func(op string, dur time.Duration)

// SlowQueryLogger is a gorm.io/gorm/logger.Interface implementation that
// emits a WARN log line and (optionally) calls observer for any query whose
// wall-clock duration exceeds Threshold. Other gorm log levels pass through
// to the supplied logrus entry at their natural levels.
type SlowQueryLogger struct {
	Threshold time.Duration
	Logger    *logrus.Entry
	Observer  SlowQueryObserver
	Level     gormlogger.LogLevel
}

// NewSlowQueryLogger builds a SlowQueryLogger with sane defaults. threshold
// of 0 disables slow-query reporting. observer may be nil.
func NewSlowQueryLogger(logger *logrus.Entry, threshold time.Duration, observer SlowQueryObserver) SlowQueryLogger {
	return SlowQueryLogger{
		Threshold: threshold,
		Logger:    logger,
		Observer:  observer,
		Level:     gormlogger.Warn,
	}
}

// LogMode returns a copy of the logger with the requested level. GORM mutates
// internals expecting a value-typed copy, so don't mutate the receiver.
func (s SlowQueryLogger) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	s.Level = level
	return s
}

// Info passes through to logrus at INFO level when GORM's level allows it.
func (s SlowQueryLogger) Info(_ context.Context, msg string, args ...any) {
	if s.Logger == nil || s.Level < gormlogger.Info {
		return
	}
	s.Logger.Infof(msg, args...)
}

// Warn passes through to logrus at WARN level when GORM's level allows it.
func (s SlowQueryLogger) Warn(_ context.Context, msg string, args ...any) {
	if s.Logger == nil || s.Level < gormlogger.Warn {
		return
	}
	s.Logger.Warnf(msg, args...)
}

// Error passes through to logrus at ERROR level when GORM's level allows it.
func (s SlowQueryLogger) Error(_ context.Context, msg string, args ...any) {
	if s.Logger == nil || s.Level < gormlogger.Error {
		return
	}
	s.Logger.Errorf(msg, args...)
}

// Trace is the hot path. GORM calls it after every SQL execution; we only
// react when the call exceeded the threshold OR returned an error other than
// gorm.ErrRecordNotFound (which is expected control flow, not a fault).
func (s SlowQueryLogger) Trace(_ context.Context, begin time.Time, fc func() (string, int64), err error) {
	if s.Threshold <= 0 || s.Logger == nil {
		return
	}
	dur := time.Since(begin)
	slow := dur >= s.Threshold
	hardErr := err != nil && !errors.Is(err, gorm.ErrRecordNotFound)

	if !slow && !hardErr {
		return
	}

	sql, rows := fc()
	op := classifyOp(sql)
	entry := s.Logger.WithFields(logrus.Fields{
		"op":       op,
		"query_ms": dur.Milliseconds(),
		"rows":     rows,
	})

	if hardErr {
		entry.WithError(err).Warn("db: query failed")
	} else if slow {
		entry.Warn("db: slow query")
	}

	if slow && s.Observer != nil {
		s.Observer(op, dur)
	}
}

// classifyOp returns a coarse op token (select|insert|update|delete|other)
// from the leading verb of a SQL statement. Keeps label cardinality bounded.
func classifyOp(sql string) string {
	trimmed := strings.TrimLeft(sql, " \t\r\n(")
	switch {
	case hasPrefixFold(trimmed, "select"):
		return "select"
	case hasPrefixFold(trimmed, "insert"):
		return "insert"
	case hasPrefixFold(trimmed, "update"):
		return "update"
	case hasPrefixFold(trimmed, "delete"):
		return "delete"
	}
	return "other"
}

func hasPrefixFold(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	return strings.EqualFold(s[:len(prefix)], prefix)
}
