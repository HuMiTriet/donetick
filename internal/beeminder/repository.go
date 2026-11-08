package beeminder

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

type BeeminderRepository struct {
	db *gorm.DB
}

func NewBeeminderRepository(db *gorm.DB) *BeeminderRepository {
	return &BeeminderRepository{db: db}
}

// GetAccountByUsername returns the account linked to a Donetick username, or
// nil when there is none.
func (r *BeeminderRepository) GetAccountByUsername(ctx context.Context, username string) (*BeeminderAccount, error) {
	var account BeeminderAccount
	err := r.db.WithContext(ctx).Where("username = ?", username).First(&account).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &account, nil
}

func (r *BeeminderRepository) GetAccountByID(ctx context.Context, id uint) (*BeeminderAccount, error) {
	var account BeeminderAccount
	if err := r.db.WithContext(ctx).First(&account, id).Error; err != nil {
		return nil, err
	}
	return &account, nil
}

// GetAccountByToken returns the account holding an auth token, or nil.
func (r *BeeminderRepository) GetAccountByToken(ctx context.Context, token string) (*BeeminderAccount, error) {
	var account BeeminderAccount
	err := r.db.WithContext(ctx).Where("token = ?", token).First(&account).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &account, nil
}

func (r *BeeminderRepository) SaveAccount(ctx context.Context, account *BeeminderAccount) error {
	return r.db.WithContext(ctx).Save(account).Error
}

func (r *BeeminderRepository) UpdateAccountFields(ctx context.Context, id uint, fields map[string]any) error {
	return r.db.WithContext(ctx).Model(&BeeminderAccount{}).Where("id = ?", id).Updates(fields).Error
}

// GetStakeByChoreID returns the stake on a chore, or nil when it has none.
func (r *BeeminderRepository) GetStakeByChoreID(ctx context.Context, choreID int) (*BeeminderStakes, error) {
	var stake BeeminderStakes
	err := r.db.WithContext(ctx).Where("chore_id = ?", choreID).First(&stake).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &stake, nil
}

func (r *BeeminderRepository) CreateStake(ctx context.Context, stake *BeeminderStakes) error {
	return r.db.WithContext(ctx).Create(stake).Error
}

func (r *BeeminderRepository) UpdateStakeAmount(ctx context.Context, stakeID uint, amountCents int) error {
	return r.db.WithContext(ctx).Model(&BeeminderStakes{}).Where("id = ?", stakeID).Update("amount_cents", amountCents).Error
}

// DeleteStake removes a stake for good. ChoreID is uniquely indexed, so a
// soft-deleted row would block placing a new stake on the same chore.
func (r *BeeminderRepository) DeleteStake(ctx context.Context, stakeID uint) error {
	return r.db.WithContext(ctx).Unscoped().Delete(&BeeminderStakes{}, stakeID).Error
}

func (r *BeeminderRepository) CreateCharge(ctx context.Context, charge *BeeminderCharges) error {
	return r.db.WithContext(ctx).Create(charge).Error
}

func (r *BeeminderRepository) GetChargesForChore(ctx context.Context, choreID int) ([]BeeminderCharges, error) {
	var charges []BeeminderCharges
	err := r.db.WithContext(ctx).Where("chore_id = ?", choreID).Order("id asc").Find(&charges).Error
	return charges, err
}

// ChargeableStake is a stake whose chore is overdue and has not been charged
// for its current due date.
type ChargeableStake struct {
	StakeID     uint
	ChoreID     int
	AccountID   uint
	AmountCents int
	ChoreName   string
	NextDueDate time.Time
}

// FindChargeableStakes lists the stakes to charge for being overdue at now.
//
// A chore's next_due_date stays put until it is completed or skipped, so
// "a charge at or after next_due_date exists" means this overdue period has
// been charged already: a daily chore left undone for a week is charged once,
// not seven times. Completing it moves next_due_date forward, which starts a
// new period. Stakes are charged whether or not their edit window has closed.
// Chores waiting for approval (status 3) are left alone, since those have
// been done.
func (r *BeeminderRepository) FindChargeableStakes(ctx context.Context, now time.Time) ([]ChargeableStake, error) {
	var rows []ChargeableStake
	err := r.db.WithContext(ctx).Raw(`
		SELECT s.id AS stake_id, s.chore_id, s.account_id, s.amount_cents,
		       c.name AS chore_name, c.next_due_date
		FROM beeminder_stakes s
		JOIN chores c ON c.id = s.chore_id
		WHERE s.deleted_at IS NULL
		  AND c.is_active = ?
		  AND c.status != ?
		  AND c.next_due_date IS NOT NULL
		  AND c.next_due_date < ?
		  AND NOT EXISTS (
		    SELECT 1 FROM beeminder_charges ch
		    WHERE ch.chore_id = s.chore_id
		      AND ch.charged_at IS NOT NULL
		      AND ch.charged_at >= c.next_due_date
		  )
		ORDER BY c.next_due_date ASC`,
		true, 3, now).Scan(&rows).Error
	return rows, err
}
