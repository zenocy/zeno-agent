package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// ConversationThread is one durable conversation a user has had against
// a card. The (CardID) → (ThreadID) mapping is 1:1 — reopening the same
// card resumes the same thread, so each card surfaces a single
// continuous transcript.
type ConversationThread struct {
	ID        string    `gorm:"primaryKey;type:text"   json:"id"`
	CardID    string    `gorm:"uniqueIndex;type:text"  json:"card_id"`
	CreatedAt time.Time `                              json:"created_at"`
	UpdatedAt time.Time `                              json:"updated_at"`
}

// ConversationTurn is one (prompt, reply) round on a thread. Position
// orders turns within a thread; the repo assigns it monotonically on
// AppendTurn so callers don't need to coordinate.
type ConversationTurn struct {
	ID        string         `gorm:"primaryKey;type:text" json:"id"`
	ThreadID  string         `gorm:"index;type:text"      json:"thread_id"`
	Position  int            `gorm:"not null"             json:"position"`
	Prompt    string         `gorm:"type:text"            json:"prompt"`
	ReplyJSON datatypes.JSON `gorm:"type:text"            json:"reply"`
	TraceID   string         `gorm:"type:text;index"      json:"trace_id"`
	CreatedAt time.Time      `                            json:"created_at"`
}

// ConversationRepo persists and reads thread + turn rows.
type ConversationRepo struct {
	DB           *gorm.DB
	ThreadsTable string // "conversation_threads" by default
	TurnsTable   string // "conversation_turns" by default
	NewID        func() string
	Now          func() time.Time
}

// Migrate runs AutoMigrate for both tables.
func (r *ConversationRepo) Migrate() error {
	if err := r.DB.Table(r.threadsTable()).AutoMigrate(&ConversationThread{}); err != nil {
		return err
	}
	return r.DB.Table(r.turnsTable()).AutoMigrate(&ConversationTurn{})
}

// GetOrCreateForCard returns the thread for cardID, creating one on first
// call. Subsequent calls return the same row so reopening a card resumes
// its transcript.
func (r *ConversationRepo) GetOrCreateForCard(ctx context.Context, cardID string) (*ConversationThread, error) {
	var t ConversationThread
	err := r.DB.WithContext(ctx).Table(r.threadsTable()).Where("card_id = ?", cardID).First(&t).Error
	if err == nil {
		return &t, nil
	}
	if err != gorm.ErrRecordNotFound {
		return nil, err
	}
	now := r.now()
	t = ConversationThread{
		ID:        r.newID(),
		CardID:    cardID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := r.DB.WithContext(ctx).Table(r.threadsTable()).Create(&t).Error; err != nil {
		return nil, err
	}
	return &t, nil
}

// ListTurns returns turns for a thread in position order.
func (r *ConversationRepo) ListTurns(ctx context.Context, threadID string) ([]ConversationTurn, error) {
	var out []ConversationTurn
	err := r.DB.WithContext(ctx).Table(r.turnsTable()).
		Where("thread_id = ?", threadID).
		Order("position ASC").
		Find(&out).Error
	return out, err
}

// AppendTurn inserts a new turn at the next position. The reply payload
// is stored as opaque JSON so SubCard schema changes don't require a
// store migration.
func (r *ConversationRepo) AppendTurn(ctx context.Context, threadID, prompt string, replyJSON []byte, traceID string) (*ConversationTurn, error) {
	var nextPos int
	row := struct{ MaxPos *int }{}
	if err := r.DB.WithContext(ctx).Table(r.turnsTable()).
		Select("MAX(position) AS max_pos").
		Where("thread_id = ?", threadID).
		Scan(&row).Error; err != nil {
		return nil, err
	}
	if row.MaxPos != nil {
		nextPos = *row.MaxPos + 1
	}
	turn := ConversationTurn{
		ID:        r.newID(),
		ThreadID:  threadID,
		Position:  nextPos,
		Prompt:    prompt,
		ReplyJSON: datatypes.JSON(replyJSON),
		TraceID:   traceID,
		CreatedAt: r.now(),
	}
	if err := r.DB.WithContext(ctx).Table(r.turnsTable()).Create(&turn).Error; err != nil {
		return nil, err
	}
	if err := r.DB.WithContext(ctx).Table(r.threadsTable()).
		Where("id = ?", threadID).
		Update("updated_at", turn.CreatedAt).Error; err != nil {
		return nil, err
	}
	return &turn, nil
}

func (r *ConversationRepo) threadsTable() string {
	if r.ThreadsTable == "" {
		return "conversation_threads"
	}
	return r.ThreadsTable
}

func (r *ConversationRepo) turnsTable() string {
	if r.TurnsTable == "" {
		return "conversation_turns"
	}
	return r.TurnsTable
}

func (r *ConversationRepo) newID() string {
	if r.NewID != nil {
		return r.NewID()
	}
	return uuid.NewString()
}

func (r *ConversationRepo) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
