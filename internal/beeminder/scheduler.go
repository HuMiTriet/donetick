package beeminder

import (
	"context"
	"fmt"
	"sync"
	"time"

	"donetick.com/core/config"
	"donetick.com/core/logging"
)

// chargeNoteTimeLayout formats the due date in overdue charge notes as
// YYYY-MM-DD:HH:MM plus the zone name. It is a Go time layout, not a date:
// Go spells formats out using its reference time (Mon Jan 2 15:04:05 MST
// 2006), so 2006 is the year, 01 the month, 02 the day, 15 the 24-hour hour,
// 04 the minute and MST the zone. It prints like "2026-11-08:23:59 UTC".
const chargeNoteTimeLayout = "2006-01-02:15:04 MST"

// Scheduler periodically charges stakes on chores that have gone overdue.
type Scheduler struct {
	svc      *Service
	repo     *BeeminderRepository
	enabled  bool
	interval time.Duration
	done     chan struct{}
	stopOnce sync.Once
	started  bool
}

func NewScheduler(svc *Service, repo *BeeminderRepository, cfg *config.Config) *Scheduler {
	interval := cfg.Beeminder.CheckInterval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return &Scheduler{
		svc:      svc,
		repo:     repo,
		enabled:  cfg.Beeminder.Enabled,
		interval: interval,
		done:     make(chan struct{}),
	}
}

// Start charges due stakes right away and then every check interval. It does
// nothing when Beeminder is turned off.
func (s *Scheduler) Start(ctx context.Context) {
	if !s.enabled {
		return
	}
	s.started = true
	logger := logging.FromContext(ctx)
	logger.Infow("Beeminder overdue scheduler started", "interval", s.interval)

	go func() {
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		for {
			s.chargeDueStakes(ctx)
			select {
			case <-s.done:
				logger.Info("Beeminder overdue scheduler stopped")
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *Scheduler) Stop() {
	if !s.started {
		return
	}
	s.stopOnce.Do(func() { close(s.done) })
}

// chargeDueStakes charges every stake whose chore is overdue and hasn't been
// charged for this overdue period, and returns how many were charged. A
// failed charge is logged and retried on the next run, since nothing is
// recorded for it.
func (s *Scheduler) chargeDueStakes(ctx context.Context) int {
	logger := logging.FromContext(ctx)
	rows, err := s.repo.FindChargeableStakes(ctx, s.svc.now().UTC())
	if err != nil {
		logger.Errorw("Failed to find overdue Beeminder stakes", "error", err)
		return 0
	}
	charged := 0
	for _, row := range rows {
		note := fmt.Sprintf("Donetick: %q was due %s and wasn't done", row.ChoreName, row.NextDueDate.UTC().Format(chargeNoteTimeLayout))
		if err := s.svc.ChargeViaBeeminder(ctx, row.ChoreID, row.AccountID, row.AmountCents, note); err != nil {
			logger.Errorw("Failed to charge overdue Beeminder stake", "choreID", row.ChoreID, "stakeID", row.StakeID, "error", err)
			continue
		}
		charged++
		logger.Infow("Charged overdue Beeminder stake", "choreID", row.ChoreID, "amountCents", row.AmountCents)
	}
	return charged
}
