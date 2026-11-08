package beeminder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"donetick.com/core/config"
	chModel "donetick.com/core/internal/chore/model"
	lModel "donetick.com/core/internal/label/model"
	lRepo "donetick.com/core/internal/label/repo"
	uModel "donetick.com/core/internal/user/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const goodToken = "good-token"

// fakeBeeminder stands in for the Beeminder API.
type fakeBeeminder struct {
	mu        sync.Mutex
	charges   []url.Values
	failNext  bool
	badBody   bool // charges succeed but answer 200 with a body that isn't JSON
	deadbeat  bool
	userLooks int
}

func (f *fakeBeeminder) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if r.Form.Get("auth_token") != goodToken {
		http.Error(w, `{"errors":"bad token"}`, http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case "/users/me.json":
		f.userLooks++
		json.NewEncoder(w).Encode(map[string]any{"username": "alice", "deadbeat": f.deadbeat})
	case "/charges.json":
		if f.failNext {
			f.failNext = false
			http.Error(w, `{"errors":"boom"}`, http.StatusInternalServerError)
			return
		}
		f.charges = append(f.charges, r.PostForm)
		if f.badBody {
			w.Write([]byte("not json"))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"id": fmt.Sprintf("ch_%d", len(f.charges)), "amount": r.PostForm.Get("amount"),
			"note": r.PostForm.Get("note"), "username": r.PostForm.Get("user_id"),
		})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeBeeminder) chargeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.charges)
}

func (f *fakeBeeminder) lastCharge() url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.charges[len(f.charges)-1]
}

type testEnv struct {
	svc       *Service
	repo      *BeeminderRepository
	scheduler *Scheduler
	db        *gorm.DB
	fake      *fakeBeeminder
	now       time.Time
	owner     *uModel.User
}

func (e *testEnv) advance(d time.Duration) { e.now = e.now.Add(d) }

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	fake := &fakeBeeminder{}
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(server.Close)

	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{
		DisableForeignKeyConstraintWhenMigrating: true,
	})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&chModel.Chore{}, &chModel.ChoreLabels{}, &lModel.Label{},
		&BeeminderAccount{}, &BeeminderStakes{}, &BeeminderCharges{},
	))

	cfg := &config.Config{}
	cfg.Database.Type = "sqlite"
	cfg.Beeminder = config.BeeminderConfig{
		Enabled:        true,
		BaseURL:        server.URL,
		TimeoutSeconds: 5,
		CheckInterval:  time.Minute,
		EditWindow:     15 * time.Minute,
		MaxAmountCents: 10000,
	}
	repo := NewBeeminderRepository(db)
	env := &testEnv{
		repo:  repo,
		db:    db,
		fake:  fake,
		now:   time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC),
		owner: &uModel.User{ID: 1, CircleID: 1, Username: "tri"},
	}
	env.svc = NewService(repo, NewClient(cfg), lRepo.NewLabelRepository(db, cfg), cfg)
	env.svc.now = func() time.Time { return env.now }
	env.scheduler = NewScheduler(env.svc, repo, cfg)
	return env
}

func (e *testEnv) link(t *testing.T) *BeeminderAccount {
	t.Helper()
	account, _, err := e.svc.LinkAccount(context.Background(), e.owner, goodToken)
	require.NoError(t, err)
	return account
}

func (e *testEnv) chore(t *testing.T, freq chModel.FrequencyType, due time.Time) *chModel.Chore {
	t.Helper()
	due = due.UTC()
	chore := &chModel.Chore{
		Name: "Gym", FrequencyType: freq, Frequency: 1, NextDueDate: &due,
		IsActive: true, CircleID: 1, CreatedBy: e.owner.ID, CreatedAt: e.now,
	}
	require.NoError(t, e.db.Create(chore).Error)
	return chore
}

func (e *testEnv) setDue(t *testing.T, choreID int, due time.Time) {
	t.Helper()
	require.NoError(t, e.db.Model(&chModel.Chore{}).Where("id = ?", choreID).Update("next_due_date", due.UTC()).Error)
}

func (e *testEnv) chargeRows(t *testing.T, choreID int) []BeeminderCharges {
	t.Helper()
	rows, err := e.repo.GetChargesForChore(context.Background(), choreID)
	require.NoError(t, err)
	return rows
}

func requireValidation(t *testing.T, err error) {
	t.Helper()
	var validation *ValidationError
	require.ErrorAs(t, err, &validation)
	require.Equal(t, http.StatusBadRequest, StatusCode(err))
}

func TestLinkAccount(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	_, _, err := env.svc.LinkAccount(ctx, env.owner, "nope")
	require.ErrorIs(t, err, ErrInvalidToken)

	account, me, err := env.svc.LinkAccount(ctx, env.owner, "  "+goodToken+" ")
	require.NoError(t, err)
	require.Equal(t, "alice", me.Username)
	require.Equal(t, "tri", *account.Username, "Username holds the Donetick username")
	require.Equal(t, 900, account.EditWindowSeconds, "copied from the YAML edit window")

	again, _, err := env.svc.LinkAccount(ctx, env.owner, goodToken)
	require.NoError(t, err)
	require.Equal(t, account.ID, again.ID, "relinking updates the same row")

	other := &uModel.User{ID: 2, CircleID: 1, Username: "bob"}
	_, _, err = env.svc.LinkAccount(ctx, other, goodToken)
	requireValidation(t, err)
}

func TestPlaceStakeValidation(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	future := env.now.Add(24 * time.Hour)
	past := env.now.Add(-time.Hour)
	chore := env.chore(t, "once", future)

	_, err := env.svc.PlaceStake(ctx, env.owner, chore.ID, &future, 500)
	requireValidation(t, err) // no linked account
	env.link(t)

	for name, tc := range map[string]struct {
		due    *time.Time
		amount int
	}{
		"no due date":    {nil, 500},
		"past due date":  {&past, 500},
		"below $1":       {&future, 99},
		"above the cap":  {&future, 10001},
		"zero is not ok": {&future, 0},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := env.svc.PlaceStake(ctx, env.owner, chore.ID, tc.due, tc.amount)
			requireValidation(t, err)
		})
	}

	stake, err := env.svc.PlaceStake(ctx, env.owner, chore.ID, &future, 550)
	require.NoError(t, err)
	require.Equal(t, 550, stake.AmountCents)
	require.Equal(t, env.now.Add(15*time.Minute), stake.EditableUntil.UTC())

	_, err = env.svc.PlaceStake(ctx, env.owner, chore.ID, &future, 550)
	requireValidation(t, err) // one stake per chore
}

func TestDefaultLabelFollowsStake(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	env.link(t)
	circleID := 1
	label := &lModel.Label{Name: "staked", Color: "#f00", CreatedBy: env.owner.ID, CircleID: &circleID}
	require.NoError(t, env.db.Create(label).Error)

	_, err := env.svc.UpdateSettings(ctx, env.owner, false, &[]int{label.ID + 100}[0])
	requireValidation(t, err) // not one of the user's labels
	account, err := env.svc.UpdateSettings(ctx, env.owner, true, &label.ID)
	require.NoError(t, err)
	require.True(t, account.DryRun)

	due := env.now.Add(time.Hour)
	chore := env.chore(t, "once", due)
	labelCount := func() int64 {
		var n int64
		env.db.Model(&chModel.ChoreLabels{}).Where("chore_id = ? AND label_id = ?", chore.ID, label.ID).Count(&n)
		return n
	}

	require.NoError(t, env.svc.ApplyStakeChange(ctx, env.owner, chore.ID, &due, 500))
	require.EqualValues(t, 1, labelCount(), "placing a stake attaches the default label")

	require.NoError(t, env.svc.ApplyStakeChange(ctx, env.owner, chore.ID, &due, 0))
	require.EqualValues(t, 0, labelCount(), "removing the stake removes the label")
	stake, err := env.repo.GetStakeByChoreID(ctx, chore.ID)
	require.NoError(t, err)
	require.Nil(t, stake)

	// Clearing the default label means new stakes get none.
	_, err = env.svc.UpdateSettings(ctx, env.owner, false, nil)
	require.NoError(t, err)
	require.NoError(t, env.svc.ApplyStakeChange(ctx, env.owner, chore.ID, &due, 500))
	require.EqualValues(t, 0, labelCount())
}

func TestEditWindowThenLocked(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	env.link(t)
	due := env.now.Add(48 * time.Hour)
	chore := env.chore(t, "daily", due)
	_, err := env.svc.PlaceStake(ctx, env.owner, chore.ID, &due, 500)
	require.NoError(t, err)

	// Inside the window: unlocked, the amount can change, others can't touch it.
	require.NoError(t, env.svc.EnsureUnlocked(ctx, chore.ID))
	require.NoError(t, env.svc.ApplyStakeChange(ctx, env.owner, chore.ID, &due, 700))
	bob := &uModel.User{ID: 2, CircleID: 1, Username: "bob"}
	requireValidation(t, env.svc.ApplyStakeChange(ctx, bob, chore.ID, &due, 0))

	env.advance(15 * time.Minute)
	err = env.svc.EnsureUnlocked(ctx, chore.ID)
	require.ErrorIs(t, err, ErrChoreLocked)
	require.Equal(t, http.StatusLocked, StatusCode(err))
	require.ErrorIs(t, env.svc.ApplyStakeChange(ctx, env.owner, chore.ID, &due, 0), ErrChoreLocked)
	require.ErrorIs(t, env.svc.ApplyStakeChange(ctx, env.owner, chore.ID, &due, 900), ErrChoreLocked)
	stake, err := env.repo.GetStakeByChoreID(ctx, chore.ID)
	require.NoError(t, err)
	require.Equal(t, 700, stake.AmountCents)

	// Chores without a stake are never locked.
	plain := env.chore(t, "daily", due)
	require.NoError(t, env.svc.EnsureUnlocked(ctx, plain.ID))
}

func TestChargeSkip(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	env.link(t)
	due := env.now.Add(48 * time.Hour)
	chore := env.chore(t, "daily", due)

	// No stake: nothing is charged.
	require.NoError(t, env.svc.ChargeSkip(ctx, chore.ID, chore.Name))
	require.Zero(t, env.fake.chargeCount())

	_, err := env.svc.PlaceStake(ctx, env.owner, chore.ID, &due, 550)
	require.NoError(t, err)

	// A failed charge moves no money and records nothing.
	env.fake.failNext = true
	err = env.svc.ChargeSkip(ctx, chore.ID, chore.Name)
	require.ErrorIs(t, err, ErrChargeFailed)
	require.Equal(t, http.StatusBadGateway, StatusCode(err))
	require.Empty(t, env.chargeRows(t, chore.ID))

	// Skipping charges, inside the window or locked.
	require.NoError(t, env.svc.ChargeSkip(ctx, chore.ID, chore.Name))
	env.advance(time.Hour)
	require.NoError(t, env.svc.ChargeSkip(ctx, chore.ID, chore.Name))
	require.Equal(t, 2, env.fake.chargeCount())

	form := env.fake.lastCharge()
	require.Equal(t, "alice", form.Get("user_id"), "charges go to the Beeminder username")
	require.Equal(t, "5.50", form.Get("amount"))
	require.Empty(t, form.Get("dryrun"))
	require.Contains(t, form.Get("note"), `skipped "Gym"`)

	rows := env.chargeRows(t, chore.ID)
	require.Len(t, rows, 2, "every charge is kept")
	require.Equal(t, "ch_2", rows[1].BeeminderChargeID)
}

func TestDryRun(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	env.link(t)
	_, err := env.svc.UpdateSettings(ctx, env.owner, true, nil)
	require.NoError(t, err)
	due := env.now.Add(time.Hour)
	chore := env.chore(t, "once", due)
	_, err = env.svc.PlaceStake(ctx, env.owner, chore.ID, &due, 100)
	require.NoError(t, err)

	require.NoError(t, env.svc.ChargeSkip(ctx, chore.ID, chore.Name))
	require.Equal(t, "true", env.fake.lastCharge().Get("dryrun"))
	rows := env.chargeRows(t, chore.ID)
	require.Len(t, rows, 1)
	require.Contains(t, rows[0].Message, "[dry run]")
}

func TestSchedulerChargesOverduePeriodOnce(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	env.link(t)
	due := env.now.Add(time.Hour)
	chore := env.chore(t, "daily", due)
	_, err := env.svc.PlaceStake(ctx, env.owner, chore.ID, &due, 500)
	require.NoError(t, err)

	require.Zero(t, env.scheduler.chargeDueStakes(ctx), "not overdue yet")

	// A daily chore left undone for a week is charged once, not seven times.
	for i := 0; i < 7; i++ {
		env.advance(24 * time.Hour)
		env.scheduler.chargeDueStakes(ctx)
	}
	require.Equal(t, 1, env.fake.chargeCount())
	require.Contains(t, env.fake.lastCharge().Get("note"), `"Gym" was due`)

	// Completing it moves the due date forward; missing that one charges again.
	env.setDue(t, chore.ID, env.now.Add(24*time.Hour))
	require.Zero(t, env.scheduler.chargeDueStakes(ctx))
	env.advance(25 * time.Hour)
	require.Equal(t, 1, env.scheduler.chargeDueStakes(ctx))
	require.Zero(t, env.scheduler.chargeDueStakes(ctx))
	require.Len(t, env.chargeRows(t, chore.ID), 2)
}

func TestSchedulerLeavesSomeStakesAlone(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	env.link(t)

	// Overdue while still inside the edit window: charged anyway.
	soon := env.now.Add(5 * time.Minute)
	windowed := env.chore(t, "once", soon)
	_, err := env.svc.PlaceStake(ctx, env.owner, windowed.ID, &soon, 500)
	require.NoError(t, err)

	due := env.now.Add(time.Hour)
	pending := env.chore(t, "once", due)
	_, err = env.svc.PlaceStake(ctx, env.owner, pending.ID, &due, 500)
	require.NoError(t, err)
	require.NoError(t, env.db.Model(&chModel.Chore{}).Where("id = ?", pending.ID).Update("status", chModel.ChoreStatusPendingApproval).Error)

	archived := env.chore(t, "once", due)
	_, err = env.svc.PlaceStake(ctx, env.owner, archived.ID, &due, 500)
	require.NoError(t, err)
	require.NoError(t, env.db.Model(&chModel.Chore{}).Where("id = ?", archived.ID).Update("is_active", false).Error)

	env.advance(10 * time.Minute)
	require.NoError(t, env.svc.EnsureUnlocked(ctx, windowed.ID), "still inside the edit window")
	require.Equal(t, 1, env.scheduler.chargeDueStakes(ctx), "overdue and editable is still charged")

	env.advance(10 * time.Minute)
	require.Zero(t, env.scheduler.chargeDueStakes(ctx), "and only once")

	env.advance(2 * time.Hour)
	require.Zero(t, env.scheduler.chargeDueStakes(ctx), "pending approval and archived chores are not charged")
}

func TestSchedulerRetriesFailedCharges(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	env.link(t)
	due := env.now.Add(time.Hour)
	chore := env.chore(t, "once", due)
	_, err := env.svc.PlaceStake(ctx, env.owner, chore.ID, &due, 500)
	require.NoError(t, err)

	env.advance(2 * time.Hour)
	env.fake.failNext = true
	require.Zero(t, env.scheduler.chargeDueStakes(ctx))
	require.Equal(t, 1, env.scheduler.chargeDueStakes(ctx), "the next run retries")
	require.Len(t, env.chargeRows(t, chore.ID), 1)
}

// A 200 from Beeminder means the money moved, even if the body can't be
// read. The charge must be recorded so the next run doesn't charge again.
func TestUnreadableChargeResponseStillCountsAsCharged(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	env.link(t)
	due := env.now.Add(time.Hour)
	chore := env.chore(t, "once", due)
	_, err := env.svc.PlaceStake(ctx, env.owner, chore.ID, &due, 500)
	require.NoError(t, err)

	env.advance(2 * time.Hour)
	env.fake.badBody = true
	require.Equal(t, 1, env.scheduler.chargeDueStakes(ctx))
	require.Zero(t, env.scheduler.chargeDueStakes(ctx), "already charged for this overdue period")
	require.Equal(t, 1, env.fake.chargeCount(), "Beeminder was charged once")

	rows := env.chargeRows(t, chore.ID)
	require.Len(t, rows, 1)
	require.Empty(t, rows[0].BeeminderChargeID, "no charge id could be read")
}

func TestSkipChargeCoversBacklog(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	env.link(t)
	due := env.now.Add(time.Hour)
	chore := env.chore(t, "daily", due)
	_, err := env.svc.PlaceStake(ctx, env.owner, chore.ID, &due, 500)
	require.NoError(t, err)

	// Three days overdue: charged once for missing it.
	env.advance(3 * 24 * time.Hour)
	require.Equal(t, 1, env.scheduler.chargeDueStakes(ctx))

	// Skipping charges again and moves the due date one day on, which is still
	// in the past. The skip charge covers that backlog, so no third charge.
	require.NoError(t, env.svc.ChargeSkip(ctx, chore.ID, chore.Name))
	env.setDue(t, chore.ID, due.Add(24*time.Hour))
	require.Zero(t, env.scheduler.chargeDueStakes(ctx))
	require.Equal(t, 2, env.fake.chargeCount())
}

func TestClientKeepsTokenOutOfErrors(t *testing.T) {
	cfg := &config.Config{}
	cfg.Beeminder.BaseURL = "http://127.0.0.1:1" // nothing listens here
	cfg.Beeminder.TimeoutSeconds = 1
	_, err := NewClient(cfg).GetBeeminderUserInfo(context.Background(), "secret-token-value")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret-token-value")
	require.False(t, errors.Is(err, ErrInvalidToken))
}
