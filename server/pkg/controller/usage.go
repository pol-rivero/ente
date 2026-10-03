package controller

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	gTime "time"

	"github.com/ente/museum/ente"
	bonus "github.com/ente/museum/ente/storagebonus"
	"github.com/ente/museum/pkg/controller/storagebonus"
	"github.com/ente/museum/pkg/controller/usercache"
	"github.com/ente/museum/pkg/repo"
	"github.com/ente/museum/pkg/utils/time"
	"github.com/ente/stacktrace"
)

type UsageController struct {
	mu                sync.Mutex
	BillingCtrl       *BillingController
	StorageBonusCtrl  *storagebonus.Controller
	UserCacheCtrl     *usercache.Controller
	UsageRepo         *repo.UsageRepository
	UserRepo          *repo.UserRepository
	FamilyRepo        *repo.FamilyRepository
	FileRepo          *repo.FileRepository
	UploadResultCache map[int64]bool
}

const lockerFreeFileLimit = 100
const lockerPaidFileLimit = 1000
const lockerFreeStorageLimit = 1 * 1024 * 1024 * 1024
const lockerPaidStorageLimit = 10 * 1024 * 1024 * 1024

const hundredMBInBytes = 100 * 1024 * 1024

type LockerLimits struct {
	IsPaid       bool
	FileLimit    int64
	StorageLimit int64
}

func GetLockerLimitsForTier(isPaid bool) LockerLimits {
	limits := LockerLimits{
		IsPaid:       isPaid,
		FileLimit:    int64(lockerFreeFileLimit),
		StorageLimit: int64(lockerFreeStorageLimit),
	}
	if isPaid {
		limits.FileLimit = int64(lockerPaidFileLimit)
		limits.StorageLimit = int64(lockerPaidStorageLimit)
	}
	return limits
}

const maxConcurrentDriveReservations = 8

// Reservation transactions hold a pooled connection while they wait for the
// quota lock. The slots bound how many can wait at once (waiting for a slot
// holds no connection), and the timeout bounds the slot wait plus the
// transaction.
var (
	driveReservationSlots   = make(chan struct{}, maxConcurrentDriveReservations)
	driveReservationTimeout = 10 * gTime.Second
)

func (c *UsageController) CanUploadFile(ctx context.Context, userID int64, size *int64, app ente.App) error {
	if app == ente.Locker {
		return c.canUploadFile(ctx, userID, size, app)
	}
	// Drive skips the cache, which keeps the Photos fast path driven by Photos checks only.
	if app == ente.Drive {
		return c.canUploadDriveFile(ctx, userID, size, nil)
	}
	if size == nil || *size < hundredMBInBytes {
		c.mu.Lock()
		canUpload, ok := c.UploadResultCache[userID]
		c.mu.Unlock()
		if ok && canUpload {
			go func() {
				_ = c.checkAndUpdateCache(ctx, userID, size, app)
			}()
			return nil
		}
	}
	return c.checkAndUpdateCache(ctx, userID, size, app)
}

// The excluded objects aren't counted as reservations: the caller adds their
// size to sizeDelta.
func (c *UsageController) CanCommitDriveObjects(ctx context.Context, userID int64, sizeDelta int64, excludedKeys []string) error {
	return c.canUploadDriveFile(ctx, userID, &sizeDelta, excludedKeys)
}

func (c *UsageController) canUploadDriveFile(ctx context.Context, userID int64, size *int64, excludedKeys []string) error {
	plan, err := c.loadQuotaPlan(ctx, userID)
	if err != nil {
		return stacktrace.Propagate(err, "")
	}
	usage, err := c.UsageRepo.GetUsageWithDriveReservations(ctx, nil, time.Microseconds(), plan.userIDs, userID, excludedKeys)
	if err != nil {
		return stacktrace.Propagate(err, "")
	}
	return c.checkSubscriptionQuota(ctx, &plan, userID, size, usage.Combined, driveQuotaReads(usage))
}

// insert runs in the transaction holding the quota lock, so concurrent upload
// starts for the same subscription can't both fit into the same free space.
func (c *UsageController) ReserveDriveUpload(ctx context.Context, userID int64, size int64, insert func(ctx context.Context, tx *sql.Tx) error) error {
	plan, err := c.loadQuotaPlan(ctx, userID)
	if err != nil {
		return stacktrace.Propagate(err, "")
	}
	// Nothing may take a second pooled connection while holding the quota lock:
	// once every connection waits for the lock, the holder would wait forever.
	if plan.bonus == nil {
		if plan.bonus, err = c.UserCacheCtrl.GetActiveStorageBonus(ctx, plan.adminID); err != nil {
			return stacktrace.Propagate(err, "failed to get storage bonus")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, driveReservationTimeout)
	defer cancel()
	release, ok := AcquireSlot(ctx, driveReservationSlots)
	if !ok {
		return stacktrace.Propagate(ente.ErrQuotaCheckBusy, "no free reservation slot")
	}
	defer release()
	err = c.reserveDriveUploadTx(ctx, &plan, userID, size, insert)
	if err != nil && !errors.Is(err, ente.ErrStorageLimitExceeded) &&
		(errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, repo.ErrQuotaLockTimeout)) {
		return stacktrace.Propagate(ente.ErrQuotaCheckBusy, "%v", err)
	}
	return stacktrace.Propagate(err, "")
}

func (c *UsageController) reserveDriveUploadTx(ctx context.Context, plan *quotaPlan, userID int64, size int64, insert func(ctx context.Context, tx *sql.Tx) error) error {
	tx, err := c.UsageRepo.DB.BeginTx(ctx, nil)
	if err != nil {
		return stacktrace.Propagate(err, "")
	}
	defer tx.Rollback()
	if err := c.UsageRepo.LockQuota(ctx, tx, plan.adminID, driveReservationTimeout); err != nil {
		return stacktrace.Propagate(err, "")
	}
	usage, err := c.UsageRepo.GetUsageWithDriveReservations(ctx, tx, time.Microseconds(), plan.userIDs, userID, nil)
	if err != nil {
		return stacktrace.Propagate(err, "")
	}
	if err := c.checkSubscriptionQuota(ctx, plan, userID, &size, usage.Combined, driveQuotaReads(usage)); err != nil {
		return stacktrace.Propagate(err, "")
	}
	if err := insert(ctx, tx); err != nil {
		return stacktrace.Propagate(err, "")
	}
	return stacktrace.Propagate(tx.Commit(), "")
}

func (c *UsageController) checkAndUpdateCache(ctx context.Context, userID int64, size *int64, app ente.App) error {
	err := c.canUploadFile(ctx, userID, size, app)
	c.mu.Lock()
	c.UploadResultCache[userID] = err == nil
	c.mu.Unlock()
	return err
}

type quotaPlan struct {
	adminID     int64
	userIDs     []int64
	memberLimit *int64
	subStorage  int64
	bonus       *bonus.ActiveStorageBonus
}

// Read lazily, so the non-Drive check keeps its query sequence.
type quotaReads struct {
	lockerUsage func() (int64, error)
	memberUsage func() (int64, error)
}

func driveQuotaReads(usage repo.DriveUsage) quotaReads {
	return quotaReads{
		lockerUsage: func() (int64, error) { return usage.Locker, nil },
		memberUsage: func() (int64, error) { return usage.Member, nil },
	}
}

func (c *UsageController) canUploadFile(ctx context.Context, userID int64, size *int64, app ente.App) error {
	plan, err := c.loadQuotaPlan(ctx, userID)
	if err != nil {
		return stacktrace.Propagate(err, "")
	}
	if app == ente.Locker {
		return c.checkLockerLimits(ctx, &plan, size)
	}
	usage, err := c.UsageRepo.GetCombinedUsage(ctx, plan.userIDs)
	if err != nil {
		return stacktrace.Propagate(err, "")
	}
	return c.checkSubscriptionQuota(ctx, &plan, userID, size, usage, quotaReads{
		lockerUsage: func() (int64, error) {
			lockerUsage, err := c.UsageRepo.GetLockerUsage(ctx, plan.userIDs)
			if err != nil {
				return 0, err
			}
			return lockerUsage.TotalUsage, nil
		},
		memberUsage: func() (int64, error) { return c.UsageRepo.GetUsage(userID) },
	})
}

func (c *UsageController) loadQuotaPlan(ctx context.Context, userID int64) (quotaPlan, error) {
	var plan quotaPlan
	familyAdminID, err := c.UserRepo.GetFamilyAdminID(userID)
	if err != nil {
		return plan, stacktrace.Propagate(err, "")
	}
	if familyAdminID != nil {
		familyMembers, err := c.FamilyRepo.GetMembersWithStatus(*familyAdminID, repo.ActiveFamilyMemberStatus)
		if err != nil {
			return plan, stacktrace.Propagate(err, "failed to fetch family members")
		}
		plan.adminID = *familyAdminID
		for _, familyMember := range familyMembers {
			plan.userIDs = append(plan.userIDs, familyMember.MemberUserID)
			if familyMember.MemberUserID == userID && familyMember.MemberUserID != *familyAdminID {
				plan.memberLimit = familyMember.StorageLimit
			}
		}
	} else {
		plan.adminID = userID
		plan.userIDs = []int64{userID}
	}

	sub, err := c.BillingCtrl.GetActiveSubscription(plan.adminID)
	if err != nil {
		if errors.Is(err, ente.ErrNoActiveSubscription) {
			bonusRes, bonErr := c.UserCacheCtrl.GetActiveStorageBonus(ctx, plan.adminID)
			if bonErr != nil {
				return plan, stacktrace.Propagate(bonErr, "failed to get bonus data")
			}
			if bonusRes.GetMaxExpiry() <= 0 {
				return plan, stacktrace.Propagate(err, "all bonus & plan expired")
			}
			plan.bonus = bonusRes
		} else {
			return plan, stacktrace.Propagate(err, "")
		}
	} else {
		plan.subStorage = sub.Storage
	}
	return plan, nil
}

func (c *UsageController) checkLockerLimits(ctx context.Context, plan *quotaPlan, size *int64) error {
	lockerUsage, err := c.UsageRepo.GetLockerUsage(ctx, plan.userIDs)
	if err != nil {
		return stacktrace.Propagate(err, "failed to fetch locker usage")
	}

	isPaidUser := false
	if err := c.BillingCtrl.HasActiveSelfOrFamilySubscription(plan.adminID, true); err == nil {
		isPaidUser = true
	}

	limits := GetLockerLimitsForTier(isPaidUser)

	if lockerUsage.TotalFileCount >= limits.FileLimit {
		return stacktrace.Propagate(&ente.ErrFileLimitReached, "")
	}

	projectedLockerUsage := lockerUsage.TotalUsage
	if size != nil {
		projectedLockerUsage += *size
	}
	if projectedLockerUsage >= limits.StorageLimit {
		return stacktrace.Propagate(ente.ErrStorageLimitExceeded, "locker storage limit exceeded (limit %d, usage %d)", limits.StorageLimit, projectedLockerUsage)
	}
	// Locker uploads should not be blocked by Photos subscription limits.
	return nil
}

func (c *UsageController) checkSubscriptionQuota(ctx context.Context, plan *quotaPlan, userID int64, size *int64, usage int64, reads quotaReads) error {
	var err error
	subStorage := plan.subStorage
	newUsage := usage

	if size != nil {
		newUsage += *size
		subStorage += StorageOverflowAboveSubscriptionLimit
	}
	if newUsage > subStorage {
		if plan.bonus == nil {
			plan.bonus, err = c.UserCacheCtrl.GetActiveStorageBonus(ctx, plan.adminID)
			if err != nil {
				return stacktrace.Propagate(err, "failed to get storage bonus")
			}
		}
		var eligibleBonus = plan.bonus.GetUsableBonus(subStorage)
		if newUsage > (subStorage + eligibleBonus) {
			lockerUsage, lUsageErr := reads.lockerUsage()
			if lUsageErr != nil {
				return stacktrace.Propagate(lUsageErr, "failed to fetch locker usage")
			}
			if (newUsage - lockerUsage) > (subStorage + eligibleBonus) {
				return stacktrace.Propagate(ente.ErrStorageLimitExceeded, "subscription Storage Limit Exceeded (limit %d, usage %d, bonus %d) for admin %d", subStorage, usage, eligibleBonus, plan.adminID)
			}
		}
	}

	if plan.adminID != userID && plan.memberLimit != nil {
		memberUsage, memberUsageErr := reads.memberUsage()
		if memberUsageErr != nil {
			return stacktrace.Propagate(memberUsageErr, "Couldn't get Members Usage")
		}
		if size != nil {
			memberUsage += *size
		}
		if memberUsage > (*plan.memberLimit + StorageOverflowAboveSubscriptionLimit) {
			return stacktrace.Propagate(ente.ErrStorageLimitExceeded, "member Storage Limit Exceeded (limit %d, usage %d)", *plan.memberLimit, memberUsage)

		}
	}
	return nil
}
