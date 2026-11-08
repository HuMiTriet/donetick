package beeminder

import (
	"gorm.io/gorm"
	"time"
)

type BeeminderAccount struct {
	gorm.Model
	Username          *string `gorm:"uniqueIndex"`
	AuthToken         string  `json:"-" gorm:"column:token;size:64;uniqueIndex"`
	EditWindowSeconds int     `gorm:"not null"`
	Deadbeat          bool    `gorm:"not null"`
	DryRun            bool    `gorm:"not null"`
	DefaultLabelID    *int
}

type BeeminderStakes struct {
	gorm.Model
	ChoreID       int       `gorm:"not null;uniqueIndex"`
	AccountID     uint      `gorm:"not null;index"`
	AmountCents   int       `gorm:"not null"`
	EditableUntil time.Time `gorm:"not null"`
}

type BeeminderCharges struct {
	ID                int        `gorm:"primarykey"`
	ChoreID           int        `gorm:"not null;index"`
	ChargedAt         *time.Time `gorm:"index"`
	BeeminderChargeID string
	Message           string
}
