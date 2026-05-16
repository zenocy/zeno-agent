package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func openConversationDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_journal_mode=WAL&_foreign_keys=on"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, (&ConversationRepo{DB: db}).Migrate())
	return db
}

func TestConversationRepo_GetOrCreateForCard_Idempotent(t *testing.T) {
	db := openConversationDB(t)
	repo := &ConversationRepo{DB: db}
	ctx := context.Background()

	first, err := repo.GetOrCreateForCard(ctx, "card-1")
	require.NoError(t, err)
	require.NotNil(t, first)
	require.Equal(t, "card-1", first.CardID)

	second, err := repo.GetOrCreateForCard(ctx, "card-1")
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID, "second call returns the same thread row")
}

func TestConversationRepo_AppendTurn_OrdersAndPositionsMonotonic(t *testing.T) {
	db := openConversationDB(t)
	repo := &ConversationRepo{DB: db}
	ctx := context.Background()

	thread, err := repo.GetOrCreateForCard(ctx, "card-1")
	require.NoError(t, err)

	t1, err := repo.AppendTurn(ctx, thread.ID, "first", []byte(`{"kind":"answer","body":"a"}`), "trace-1")
	require.NoError(t, err)
	require.Equal(t, 0, t1.Position)

	t2, err := repo.AppendTurn(ctx, thread.ID, "second", []byte(`{"kind":"answer","body":"b"}`), "trace-2")
	require.NoError(t, err)
	require.Equal(t, 1, t2.Position)

	turns, err := repo.ListTurns(ctx, thread.ID)
	require.NoError(t, err)
	require.Len(t, turns, 2)
	require.Equal(t, "first", turns[0].Prompt)
	require.Equal(t, "second", turns[1].Prompt)
	require.Equal(t, "trace-1", turns[0].TraceID)
	require.Equal(t, "trace-2", turns[1].TraceID)
}

func TestConversationRepo_ListTurns_EmptyThreadReturnsEmpty(t *testing.T) {
	db := openConversationDB(t)
	repo := &ConversationRepo{DB: db}
	ctx := context.Background()

	thread, err := repo.GetOrCreateForCard(ctx, "card-1")
	require.NoError(t, err)

	turns, err := repo.ListTurns(ctx, thread.ID)
	require.NoError(t, err)
	require.Empty(t, turns)
}

func TestConversationRepo_DistinctCardsHaveDistinctThreads(t *testing.T) {
	db := openConversationDB(t)
	repo := &ConversationRepo{DB: db}
	ctx := context.Background()

	a, err := repo.GetOrCreateForCard(ctx, "card-a")
	require.NoError(t, err)
	b, err := repo.GetOrCreateForCard(ctx, "card-b")
	require.NoError(t, err)
	require.NotEqual(t, a.ID, b.ID)
}
