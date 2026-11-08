package beeminder

import "gorm.io/gorm"

type BeeminderRepository struct {
	db *gorm.DB
}
