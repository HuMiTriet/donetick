package beeminder

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"donetick.com/core/config"
	lModel "donetick.com/core/internal/label/model"
	lRepo "donetick.com/core/internal/label/repo"
	uModel "donetick.com/core/internal/user/model"
	"donetick.com/core/logging"
	"gorm.io/gorm"
)

// MinAmountCents is Beeminder's smallest allowed charge ($1.00).
const MinAmountCents = 100

var (
	// ErrChoreLocked means the chore carries a stake whose edit window has
	// closed: it can no longer be edited, deleted or archived.
	ErrChoreLocked = errors.New("this task is locked by a Beeminder stake and can no longer be changed or deleted")
	// ErrChargeFailed means Beeminder could not be charged.
	ErrChargeFailed = errors.New("couldn't charge Beeminder")
)

// ValidationError is a problem with a request that the user can fix.
type ValidationError struct{ msg string }

func (e *ValidationError) Error() string { return e.msg }

func invalid(format string, args ...any) error {
	return &ValidationError{msg: fmt.Sprintf(format, args...)}
}

// StatusCode maps an error from the Service to an HTTP status code.
func StatusCode(err error) int {
	var validation *ValidationError
	switch {
	case errors.Is(err, ErrChoreLocked):
		return http.StatusLocked
	case errors.As(err, &validation):
		return http.StatusBadRequest
	case errors.Is(err, ErrChargeFailed):
		return http.StatusBadGateway
	}
	return http.StatusInternalServerError
}

// Service holds the rules for Beeminder accounts, stakes and charges.
type Service struct {
	repo   *BeeminderRepository
	client *Client
	labels *lRepo.LabelRepository
	cfg    config.BeeminderConfig
	now    func() time.Time
}

func NewService(repo *BeeminderRepository, client *Client, labels *lRepo.LabelRepository, cfg *config.Config) *Service {
	return &Service{
		repo:   repo,
		client: client,
		labels: labels,
		cfg:    cfg.Beeminder,
		now:    time.Now,
	}
}

func (s *Service) Enabled() bool { return s.cfg.Enabled }

func (s *Service) EditWindow() time.Duration { return s.cfg.EditWindow }

func (s *Service) MaxAmountCents() int { return s.cfg.MaxAmountCents }

// DryRun reports whether charges on account are simulated. Only the server
// decides this: the YAML dry_run setting covers every account, and an
// account's dry_run column can be set directly in the database. Clients can't
// change either.
func (s *Service) DryRun(account *BeeminderAccount) bool {
	return s.cfg.DryRun || account.DryRun
}

// GetAccount returns the Beeminder account linked to a Donetick user, or nil.
func (s *Service) GetAccount(ctx context.Context, userID int) (*BeeminderAccount, error) {
	return s.repo.GetAccountByDoneTickUserID(ctx, userID)
}

// BeeminderUser returns the Beeminder user behind an account's token.
func (s *Service) BeeminderUser(ctx context.Context, account *BeeminderAccount) (*User, error) {
	me, err := s.client.GetBeeminderUserInfo(ctx, account.AuthToken)
	if err != nil {
		return nil, err
	}
	if me.Deadbeat != account.Deadbeat {
		if err := s.repo.UpdateAccountFields(ctx, account.ID, map[string]any{"deadbeat": me.Deadbeat}); err == nil {
			account.Deadbeat = me.Deadbeat
		}
	}
	return me, nil
}

// LinkAccount checks token with Beeminder and links it to the Donetick user,
// storing the Beeminder username it belongs to. It returns ErrInvalidToken
// when Beeminder rejects the token. A token or Beeminder account can only be
// linked to one Donetick user.
func (s *Service) LinkAccount(ctx context.Context, user *uModel.User, token string) (*BeeminderAccount, *User, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, nil, invalid("Enter your Beeminder auth token")
	}
	if len(token) > 64 {
		return nil, nil, ErrInvalidToken
	}
	me, err := s.client.GetBeeminderUserInfo(ctx, token)
	if err != nil {
		return nil, nil, err
	}

	other, err := s.repo.GetAccountByToken(ctx, token)
	if err != nil {
		return nil, nil, err
	}
	if other != nil && other.DoneTickUserID != user.ID {
		return nil, nil, invalid("This Beeminder auth token is already linked to another Donetick account")
	}
	other, err = s.repo.GetAccountByUsername(ctx, me.Username)
	if err != nil {
		return nil, nil, err
	}
	if other != nil && other.DoneTickUserID != user.ID {
		return nil, nil, invalid("This Beeminder account is already linked to another Donetick account")
	}

	account, err := s.repo.GetAccountByDoneTickUserID(ctx, user.ID)
	if err != nil {
		return nil, nil, err
	}
	if account == nil {
		account = &BeeminderAccount{DoneTickUserID: user.ID}
	}
	account.Username = &me.Username
	account.AuthToken = token
	account.Deadbeat = me.Deadbeat
	account.EditWindowSeconds = int(s.cfg.EditWindow.Seconds())
	if err := s.repo.SaveAccount(ctx, account); err != nil {
		return nil, nil, err
	}
	return account, me, nil
}

// UpdateSettings sets the account's default stake label. A nil
// defaultLabelID means staked chores get no label. Dry run isn't a user
// setting; see DryRun.
func (s *Service) UpdateSettings(ctx context.Context, user *uModel.User, defaultLabelID *int) (*BeeminderAccount, error) {
	account, err := s.repo.GetAccountByDoneTickUserID(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	if account == nil {
		return nil, invalid("Link your Beeminder account first")
	}

	labelValue := any(gorm.Expr("NULL"))
	if defaultLabelID != nil {
		labels, err := s.labels.GetUserLabels(ctx, user.ID, user.CircleID)
		if err != nil {
			return nil, err
		}
		owned := slices.ContainsFunc(labels, func(l *lModel.Label) bool { return l.ID == *defaultLabelID })
		if !owned {
			return nil, invalid("Pick one of your labels")
		}
		labelValue = *defaultLabelID
	}

	if err := s.repo.UpdateAccountFields(ctx, account.ID, map[string]any{
		"default_label_id": labelValue,
	}); err != nil {
		return nil, err
	}
	account.DefaultLabelID = defaultLabelID
	return account, nil
}

func (s *Service) checkAmount(amountCents int) error {
	if amountCents < MinAmountCents {
		return invalid("The smallest stake Beeminder allows is $%s", FormatDollars(MinAmountCents))
	}
	if s.cfg.MaxAmountCents > 0 && amountCents > s.cfg.MaxAmountCents {
		return invalid("The largest stake allowed is $%s", FormatDollars(s.cfg.MaxAmountCents))
	}
	return nil
}

func (s *Service) CheckNewStake(ctx context.Context, user *uModel.User, nextDueDate *time.Time, amountCents int) (*BeeminderAccount, error) {
	if !s.cfg.Enabled {
		return nil, invalid("Beeminder stakes are turned off on this server")
	}
	if nextDueDate == nil {
		return nil, invalid("A stake can only be placed on a task with a due date")
	}
	if !nextDueDate.After(s.now()) {
		return nil, invalid("A stake needs a due date in the future")
	}
	if err := s.checkAmount(amountCents); err != nil {
		return nil, err
	}
	account, err := s.repo.GetAccountByDoneTickUserID(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	if account == nil {
		return nil, invalid("Link your Beeminder account in Settings before placing a stake")
	}
	return account, nil
}

// PlaceStake puts amountCents on choreID. The chore stays editable for the
// configured edit window and then locks. The account's default label, if
// any, is attached to the chore.
func (s *Service) PlaceStake(ctx context.Context, user *uModel.User, choreID int, nextDueDate *time.Time, amountCents int) (*BeeminderStakes, error) {
	account, err := s.CheckNewStake(ctx, user, nextDueDate, amountCents)
	if err != nil {
		return nil, err
	}
	existing, err := s.repo.GetStakeByChoreID(ctx, choreID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, invalid("This task already has a stake")
	}

	stake := &BeeminderStakes{
		ChoreID:       choreID,
		AccountID:     account.ID,
		AmountCents:   amountCents,
		EditableUntil: s.now().UTC().Add(s.cfg.EditWindow),
	}
	if err := s.repo.CreateStake(ctx, stake); err != nil {
		return nil, err
	}

	if account.DefaultLabelID != nil {
		if err := s.labels.AssignLabelsToChore(ctx, choreID, user.ID, user.CircleID, []int{*account.DefaultLabelID}, nil); err != nil {
			// The label may have been deleted since it was chosen. The stake
			// matters more than the label, so keep going.
			logging.FromContext(ctx).Warnw("Couldn't attach the default Beeminder label", "choreID", choreID, "labelID", *account.DefaultLabelID, "error", err)
		}
	}
	return stake, nil
}

// ApplyStakeChange applies a stake change from the edit form: 0 removes the
// stake, any other amount places one or changes its amount.
func (s *Service) ApplyStakeChange(ctx context.Context, user *uModel.User, choreID int, nextDueDate *time.Time, amountCents int) error {
	stake, err := s.repo.GetStakeByChoreID(ctx, choreID)
	if err != nil {
		return err
	}
	if amountCents == 0 {
		if stake == nil {
			return nil
		}
		return s.RemoveStake(ctx, user, stake)
	}
	if stake == nil {
		_, err := s.PlaceStake(ctx, user, choreID, nextDueDate, amountCents)
		return err
	}
	if stake.AmountCents == amountCents {
		return nil
	}
	if _, err := s.checkEditable(ctx, user, stake); err != nil {
		return err
	}
	if err := s.checkAmount(amountCents); err != nil {
		return err
	}
	return s.repo.UpdateStakeAmount(ctx, stake.ID, amountCents)
}

// checkEditable allows changes to a stake only inside its edit window and
// only by the person who placed it.
func (s *Service) checkEditable(ctx context.Context, user *uModel.User, stake *BeeminderStakes) (*BeeminderAccount, error) {
	if !s.now().Before(stake.EditableUntil) {
		return nil, ErrChoreLocked
	}
	account, err := s.repo.GetAccountByID(ctx, stake.AccountID)
	if err != nil {
		return nil, err
	}
	if account.DoneTickUserID != user.ID {
		return nil, invalid("Only the person who placed this stake can change it")
	}
	return account, nil
}

// RemoveStake takes the stake off a chore during its edit window, along with
// the default label it brought.
func (s *Service) RemoveStake(ctx context.Context, user *uModel.User, stake *BeeminderStakes) error {
	account, err := s.checkEditable(ctx, user, stake)
	if err != nil {
		return err
	}
	if err := s.repo.DeleteStake(ctx, stake.ID); err != nil {
		return err
	}
	if account.DefaultLabelID != nil {
		if err := s.labels.DeassignLabelsFromChore(ctx, stake.ChoreID, user.ID, []int{*account.DefaultLabelID}); err != nil {
			logging.FromContext(ctx).Warnw("Couldn't remove the default Beeminder label", "choreID", stake.ChoreID, "error", err)
		}
	}
	return nil
}

// EnsureUnlocked returns ErrChoreLocked when choreID carries a stake whose
// edit window has closed.
func (s *Service) EnsureUnlocked(ctx context.Context, choreID int) error {
	stake, err := s.repo.GetStakeByChoreID(ctx, choreID)
	if err != nil {
		return err
	}
	if stake != nil && !s.now().Before(stake.EditableUntil) {
		return ErrChoreLocked
	}
	return nil
}

// ChargeSkip charges the stake on choreID, if it has one, for being skipped.
// Skipping always charges, locked or not. An error wrapping ErrChargeFailed
// means no money moved; any other error means the charge went through but
// could not be recorded.
func (s *Service) ChargeSkip(ctx context.Context, choreID int, choreName string) error {
	stake, err := s.repo.GetStakeByChoreID(ctx, choreID)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrChargeFailed, err)
	}
	if stake == nil {
		return nil
	}
	note := fmt.Sprintf("Donetick: skipped %q", choreName)
	return s.ChargeViaBeeminder(ctx, stake.ChoreID, stake.AccountID, stake.AmountCents, note)
}

// ChargeViaBeeminder charges a stake through Beeminder and records the ChargeViaBeeminder. ChargeSkip
// and the overdue Scheduler both go through here.
func (s *Service) ChargeViaBeeminder(ctx context.Context, choreID int, accountID uint, amountCents int, note string) error {
	account, err := s.repo.GetAccountByID(ctx, accountID)
	if err != nil {
		return fmt.Errorf("%w: loading account: %w", ErrChargeFailed, err)
	}
	me, err := s.BeeminderUser(ctx, account)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrChargeFailed, err)
	}
	dryRun := s.DryRun(account)
	chargeID, err := s.client.Charge(ctx, account.AuthToken, me.Username, amountCents, note, dryRun)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrChargeFailed, err)
	}

	chargedAt := s.now().UTC()
	message := note
	if dryRun {
		message = "[dry run] " + note
	}
	if err := s.repo.CreateCharge(ctx, &BeeminderCharges{
		ChoreID:           choreID,
		ChargedAt:         &chargedAt,
		BeeminderChargeID: chargeID,
		Message:           message,
	}); err != nil {
		return fmt.Errorf("beeminder charged $%s for chore %d but recording it failed: %w", FormatDollars(amountCents), choreID, err)
	}
	return nil
}
