package store

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

type observation struct {
	op  string
	dur time.Duration
}

func newTestLogger() (*logrus.Entry, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	l := logrus.New()
	l.Out = buf
	l.Level = logrus.DebugLevel
	l.Formatter = &logrus.TextFormatter{DisableColors: true, DisableTimestamp: true}
	return logrus.NewEntry(l), buf
}

func TestSlowQueryLogger_BelowThreshold_NoLog(t *testing.T) {
	entry, buf := newTestLogger()
	var got []observation
	sql := NewSlowQueryLogger(entry, 50*time.Millisecond, func(op string, d time.Duration) {
		got = append(got, observation{op, d})
	})

	begin := time.Now().Add(-10 * time.Millisecond)
	sql.Trace(context.Background(), begin, func() (string, int64) {
		return "SELECT * FROM events", 1
	}, nil)

	if buf.Len() != 0 {
		t.Errorf("expected no log output, got %q", buf.String())
	}
	if len(got) != 0 {
		t.Errorf("expected no observations, got %d", len(got))
	}
}

func TestSlowQueryLogger_AboveThreshold_LogsAndObserves(t *testing.T) {
	entry, buf := newTestLogger()
	var got []observation
	sql := NewSlowQueryLogger(entry, 5*time.Millisecond, func(op string, d time.Duration) {
		got = append(got, observation{op, d})
	})

	begin := time.Now().Add(-25 * time.Millisecond)
	sql.Trace(context.Background(), begin, func() (string, int64) {
		return "SELECT * FROM events WHERE kind = ?", 12
	}, nil)

	out := buf.String()
	if !strings.Contains(out, "slow query") {
		t.Fatalf("expected slow-query log, got: %q", out)
	}
	if !strings.Contains(out, "op=select") {
		t.Errorf("expected op=select in log, got: %q", out)
	}
	if len(got) != 1 || got[0].op != "select" {
		t.Errorf("expected observer call op=select, got %+v", got)
	}
}

func TestSlowQueryLogger_HardError_LogsRegardlessOfDuration(t *testing.T) {
	entry, buf := newTestLogger()
	sql := NewSlowQueryLogger(entry, 1*time.Second, nil)

	begin := time.Now()
	sql.Trace(context.Background(), begin, func() (string, int64) {
		return "INSERT INTO events VALUES (?)", 0
	}, errors.New("disk i/o timeout"))

	out := buf.String()
	if !strings.Contains(out, "query failed") {
		t.Fatalf("expected hard-error log, got: %q", out)
	}
	if !strings.Contains(out, "op=insert") {
		t.Errorf("expected op=insert classification, got: %q", out)
	}
}

func TestSlowQueryLogger_RecordNotFound_Ignored(t *testing.T) {
	entry, buf := newTestLogger()
	sql := NewSlowQueryLogger(entry, 1*time.Second, nil)

	begin := time.Now()
	sql.Trace(context.Background(), begin, func() (string, int64) {
		return "SELECT * FROM cards LIMIT 1", 0
	}, gorm.ErrRecordNotFound)

	if buf.Len() != 0 {
		t.Errorf("ErrRecordNotFound should not log, got: %q", buf.String())
	}
}

func TestClassifyOp(t *testing.T) {
	cases := map[string]string{
		"SELECT * FROM x":         "select",
		"select * from x":         "select",
		"  insert into x":         "insert",
		"UPDATE x SET":            "update",
		"DELETE FROM x":           "delete",
		"PRAGMA journal_mode=WAL": "other",
		"(SELECT 1)":              "select",
	}
	for sql, want := range cases {
		if got := classifyOp(sql); got != want {
			t.Errorf("classifyOp(%q) = %q, want %q", sql, got, want)
		}
	}
}
