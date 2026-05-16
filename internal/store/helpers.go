package store

import (
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// firstOrNil runs q.First(dest) and turns gorm.ErrRecordNotFound into a
// nil result. Repos use this so a "missing row" looks the same to callers
// regardless of whether they care about the specific error type.
func firstOrNil[T any](q *gorm.DB, dest *T) (*T, error) {
	err := q.First(dest).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return dest, nil
}

// onConflictUpdateAll builds the GORM ON CONFLICT clause used by repo
// upsert paths that want a conflicting row fully overwritten.
func onConflictUpdateAll(cols ...string) clause.OnConflict {
	cc := make([]clause.Column, len(cols))
	for i, c := range cols {
		cc[i] = clause.Column{Name: c}
	}
	return clause.OnConflict{Columns: cc, UpdateAll: true}
}

// onConflictDoNothing builds the GORM ON CONFLICT clause used by repo
// paths that want a conflicting row left in place (idempotent insert).
func onConflictDoNothing(cols ...string) clause.OnConflict {
	cc := make([]clause.Column, len(cols))
	for i, c := range cols {
		cc[i] = clause.Column{Name: c}
	}
	return clause.OnConflict{Columns: cc, DoNothing: true}
}
