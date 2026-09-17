package db

import (
	"errors"
	"testing"
	"time"
)

func TestCreatePatternChange(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	t.Run("with defaults", func(t *testing.T) {
		pc := &PatternChange{
			Tier:       "high",
			Pattern:    "*.log",
			ChangeType: PatternChangeTypeAdd,
			Reason:     "Exclude log files",
		}

		err := db.CreatePatternChange(pc)
		if err != nil {
			t.Fatalf("CreatePatternChange failed: %v", err)
		}

		if pc.ID == 0 {
			t.Error("Expected ID to be set")
		}
		if pc.Status != PatternChangeStatusPending {
			t.Errorf("Expected status %q, got %q", PatternChangeStatusPending, pc.Status)
		}
		if pc.CreatedAt.IsZero() {
			t.Error("Expected CreatedAt to be set")
		}
	})

	t.Run("with explicit values", func(t *testing.T) {
		createdAt := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
		pc := &PatternChange{
			Tier:       "medium",
			Pattern:    "*.tmp",
			ChangeType: PatternChangeTypeRemove,
			Reason:     "Remove temp files",
			Status:     PatternChangeStatusApproved,
			CreatedAt:  createdAt,
		}

		err := db.CreatePatternChange(pc)
		if err != nil {
			t.Fatalf("CreatePatternChange failed: %v", err)
		}

		if pc.ID == 0 {
			t.Error("Expected ID to be set")
		}
		if pc.Status != PatternChangeStatusApproved {
			t.Errorf("Expected status %q, got %q", PatternChangeStatusApproved, pc.Status)
		}
		if !pc.CreatedAt.Equal(createdAt) {
			t.Errorf("Expected CreatedAt %v, got %v", createdAt, pc.CreatedAt)
		}
	})

	t.Run("all change types", func(t *testing.T) {
		changeTypes := []string{PatternChangeTypeAdd, PatternChangeTypeRemove, PatternChangeTypeSuggest}
		for _, ct := range changeTypes {
			pc := &PatternChange{
				Tier:       "low",
				Pattern:    "test-" + ct,
				ChangeType: ct,
				Reason:     "Test " + ct,
			}
			err := db.CreatePatternChange(pc)
			if err != nil {
				t.Errorf("CreatePatternChange failed for type %s: %v", ct, err)
			}
		}
	})
}

func TestGetPatternChange(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	t.Run("existing pattern change", func(t *testing.T) {
		pc := &PatternChange{
			Tier:       "high",
			Pattern:    "*.bak",
			ChangeType: PatternChangeTypeAdd,
			Reason:     "Exclude backup files",
		}
		err := db.CreatePatternChange(pc)
		if err != nil {
			t.Fatalf("CreatePatternChange failed: %v", err)
		}

		retrieved, err := db.GetPatternChange(pc.ID)
		if err != nil {
			t.Fatalf("GetPatternChange failed: %v", err)
		}

		if retrieved.ID != pc.ID {
			t.Errorf("Expected ID %d, got %d", pc.ID, retrieved.ID)
		}
		if retrieved.Tier != pc.Tier {
			t.Errorf("Expected Tier %q, got %q", pc.Tier, retrieved.Tier)
		}
		if retrieved.Pattern != pc.Pattern {
			t.Errorf("Expected Pattern %q, got %q", pc.Pattern, retrieved.Pattern)
		}
		if retrieved.ChangeType != pc.ChangeType {
			t.Errorf("Expected ChangeType %q, got %q", pc.ChangeType, retrieved.ChangeType)
		}
		if retrieved.Reason != pc.Reason {
			t.Errorf("Expected Reason %q, got %q", pc.Reason, retrieved.Reason)
		}
		if retrieved.Status != pc.Status {
			t.Errorf("Expected Status %q, got %q", pc.Status, retrieved.Status)
		}
	})

	t.Run("non-existent pattern change", func(t *testing.T) {
		_, err := db.GetPatternChange(99999)
		if !errors.Is(err, ErrPatternChangeNotFound) {
			t.Errorf("Expected ErrPatternChangeNotFound, got %v", err)
		}
	})
}

func TestListPendingPatternChanges(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Create pattern changes with different statuses
	pending1 := &PatternChange{
		Tier:       "high",
		Pattern:    "pending1.txt",
		ChangeType: PatternChangeTypeAdd,
		Reason:     "Pending 1",
		Status:     PatternChangeStatusPending,
	}
	pending2 := &PatternChange{
		Tier:       "medium",
		Pattern:    "pending2.txt",
		ChangeType: PatternChangeTypeRemove,
		Reason:     "Pending 2",
		Status:     PatternChangeStatusPending,
	}
	approved := &PatternChange{
		Tier:       "low",
		Pattern:    "approved.txt",
		ChangeType: PatternChangeTypeAdd,
		Reason:     "Approved",
		Status:     PatternChangeStatusApproved,
	}
	rejected := &PatternChange{
		Tier:       "low",
		Pattern:    "rejected.txt",
		ChangeType: PatternChangeTypeAdd,
		Reason:     "Rejected",
		Status:     PatternChangeStatusRejected,
	}

	for _, pc := range []*PatternChange{pending1, pending2, approved, rejected} {
		if err := db.CreatePatternChange(pc); err != nil {
			t.Fatalf("CreatePatternChange failed: %v", err)
		}
	}

	changes, err := db.ListPendingPatternChanges()
	if err != nil {
		t.Fatalf("ListPendingPatternChanges failed: %v", err)
	}

	if len(changes) != 2 {
		t.Errorf("Expected 2 pending changes, got %d", len(changes))
	}

	for _, pc := range changes {
		if pc.Status != PatternChangeStatusPending {
			t.Errorf("Expected pending status, got %q", pc.Status)
		}
	}
}

func TestListPatternChangesByStatus(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Create pattern changes with different statuses
	statuses := []string{
		PatternChangeStatusPending,
		PatternChangeStatusPending,
		PatternChangeStatusApproved,
		PatternChangeStatusApproved,
		PatternChangeStatusApproved,
		PatternChangeStatusRejected,
	}

	for i, status := range statuses {
		pc := &PatternChange{
			Tier:       "high",
			Pattern:    "pattern" + string(rune('A'+i)),
			ChangeType: PatternChangeTypeAdd,
			Reason:     "Test",
			Status:     status,
		}
		if err := db.CreatePatternChange(pc); err != nil {
			t.Fatalf("CreatePatternChange failed: %v", err)
		}
	}

	t.Run("pending", func(t *testing.T) {
		changes, err := db.ListPatternChangesByStatus(PatternChangeStatusPending)
		if err != nil {
			t.Fatalf("ListPatternChangesByStatus failed: %v", err)
		}
		if len(changes) != 2 {
			t.Errorf("Expected 2 pending, got %d", len(changes))
		}
	})

	t.Run("approved", func(t *testing.T) {
		changes, err := db.ListPatternChangesByStatus(PatternChangeStatusApproved)
		if err != nil {
			t.Fatalf("ListPatternChangesByStatus failed: %v", err)
		}
		if len(changes) != 3 {
			t.Errorf("Expected 3 approved, got %d", len(changes))
		}
	})

	t.Run("rejected", func(t *testing.T) {
		changes, err := db.ListPatternChangesByStatus(PatternChangeStatusRejected)
		if err != nil {
			t.Fatalf("ListPatternChangesByStatus failed: %v", err)
		}
		if len(changes) != 1 {
			t.Errorf("Expected 1 rejected, got %d", len(changes))
		}
	})

	t.Run("empty result", func(t *testing.T) {
		changes, err := db.ListPatternChangesByStatus("nonexistent")
		if err != nil {
			t.Fatalf("ListPatternChangesByStatus failed: %v", err)
		}
		if len(changes) != 0 {
			t.Errorf("Expected 0 changes, got %d", len(changes))
		}
	})
}

func TestListPatternChangesByType(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Create pattern changes with different types
	types := []string{
		PatternChangeTypeAdd,
		PatternChangeTypeAdd,
		PatternChangeTypeRemove,
		PatternChangeTypeSuggest,
		PatternChangeTypeSuggest,
		PatternChangeTypeSuggest,
	}

	for i, changeType := range types {
		pc := &PatternChange{
			Tier:       "high",
			Pattern:    "type-pattern" + string(rune('A'+i)),
			ChangeType: changeType,
			Reason:     "Test",
		}
		if err := db.CreatePatternChange(pc); err != nil {
			t.Fatalf("CreatePatternChange failed: %v", err)
		}
	}

	t.Run("add type", func(t *testing.T) {
		changes, err := db.ListPatternChangesByType(PatternChangeTypeAdd)
		if err != nil {
			t.Fatalf("ListPatternChangesByType failed: %v", err)
		}
		if len(changes) != 2 {
			t.Errorf("Expected 2 add types, got %d", len(changes))
		}
	})

	t.Run("remove type", func(t *testing.T) {
		changes, err := db.ListPatternChangesByType(PatternChangeTypeRemove)
		if err != nil {
			t.Fatalf("ListPatternChangesByType failed: %v", err)
		}
		if len(changes) != 1 {
			t.Errorf("Expected 1 remove type, got %d", len(changes))
		}
	})

	t.Run("suggest type", func(t *testing.T) {
		changes, err := db.ListPatternChangesByType(PatternChangeTypeSuggest)
		if err != nil {
			t.Fatalf("ListPatternChangesByType failed: %v", err)
		}
		if len(changes) != 3 {
			t.Errorf("Expected 3 suggest types, got %d", len(changes))
		}
	})

	t.Run("empty result", func(t *testing.T) {
		changes, err := db.ListPatternChangesByType("nonexistent")
		if err != nil {
			t.Fatalf("ListPatternChangesByType failed: %v", err)
		}
		if len(changes) != 0 {
			t.Errorf("Expected 0 changes, got %d", len(changes))
		}
	})
}

func TestListAllPatternChanges(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	t.Run("empty database", func(t *testing.T) {
		changes, err := db.ListAllPatternChanges()
		if err != nil {
			t.Fatalf("ListAllPatternChanges failed: %v", err)
		}
		if len(changes) != 0 {
			t.Errorf("Expected 0 changes in empty db, got %d", len(changes))
		}
	})

	// Create pattern changes
	for i := 0; i < 5; i++ {
		pc := &PatternChange{
			Tier:       "high",
			Pattern:    "all-pattern" + string(rune('A'+i)),
			ChangeType: PatternChangeTypeAdd,
			Reason:     "Test",
		}
		if err := db.CreatePatternChange(pc); err != nil {
			t.Fatalf("CreatePatternChange failed: %v", err)
		}
	}

	t.Run("with data", func(t *testing.T) {
		changes, err := db.ListAllPatternChanges()
		if err != nil {
			t.Fatalf("ListAllPatternChanges failed: %v", err)
		}
		if len(changes) != 5 {
			t.Errorf("Expected 5 changes, got %d", len(changes))
		}
	})
}

func TestUpdatePatternChangeStatus(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	t.Run("update existing", func(t *testing.T) {
		pc := &PatternChange{
			Tier:       "high",
			Pattern:    "update-test.txt",
			ChangeType: PatternChangeTypeAdd,
			Reason:     "Test update",
			Status:     PatternChangeStatusPending,
		}
		if err := db.CreatePatternChange(pc); err != nil {
			t.Fatalf("CreatePatternChange failed: %v", err)
		}

		err := db.UpdatePatternChangeStatus(pc.ID, PatternChangeStatusApproved)
		if err != nil {
			t.Fatalf("UpdatePatternChangeStatus failed: %v", err)
		}

		retrieved, err := db.GetPatternChange(pc.ID)
		if err != nil {
			t.Fatalf("GetPatternChange failed: %v", err)
		}
		if retrieved.Status != PatternChangeStatusApproved {
			t.Errorf("Expected status %q, got %q", PatternChangeStatusApproved, retrieved.Status)
		}
	})

	t.Run("update non-existent", func(t *testing.T) {
		err := db.UpdatePatternChangeStatus(99999, PatternChangeStatusApproved)
		if !errors.Is(err, ErrPatternChangeNotFound) {
			t.Errorf("Expected ErrPatternChangeNotFound, got %v", err)
		}
	})
}

func TestApprovePatternChange(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	t.Run("approve existing", func(t *testing.T) {
		pc := &PatternChange{
			Tier:       "dangerous",
			Pattern:    "approve-test.txt",
			ChangeType: PatternChangeTypeAdd,
			Reason:     "Test approve",
			Status:     PatternChangeStatusPending,
		}
		if err := db.CreatePatternChange(pc); err != nil {
			t.Fatalf("CreatePatternChange failed: %v", err)
		}

		err := db.ApprovePatternChange(pc.ID)
		if err != nil {
			t.Fatalf("ApprovePatternChange failed: %v", err)
		}

		retrieved, err := db.GetPatternChange(pc.ID)
		if err != nil {
			t.Fatalf("GetPatternChange failed: %v", err)
		}
		if retrieved.Status != PatternChangeStatusApproved {
			t.Errorf("Expected status %q, got %q", PatternChangeStatusApproved, retrieved.Status)
		}
	})

	t.Run("approve non-existent", func(t *testing.T) {
		err := db.ApprovePatternChange(99999)
		if !errors.Is(err, ErrPatternChangeNotFound) {
			t.Errorf("Expected ErrPatternChangeNotFound, got %v", err)
		}
	})
}

func TestRejectPatternChange(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	t.Run("reject existing", func(t *testing.T) {
		pc := &PatternChange{
			Tier:       "high",
			Pattern:    "reject-test.txt",
			ChangeType: PatternChangeTypeAdd,
			Reason:     "Test reject",
			Status:     PatternChangeStatusPending,
		}
		if err := db.CreatePatternChange(pc); err != nil {
			t.Fatalf("CreatePatternChange failed: %v", err)
		}

		err := db.RejectPatternChange(pc.ID)
		if err != nil {
			t.Fatalf("RejectPatternChange failed: %v", err)
		}

		retrieved, err := db.GetPatternChange(pc.ID)
		if err != nil {
			t.Fatalf("GetPatternChange failed: %v", err)
		}
		if retrieved.Status != PatternChangeStatusRejected {
			t.Errorf("Expected status %q, got %q", PatternChangeStatusRejected, retrieved.Status)
		}
	})

	t.Run("reject non-existent", func(t *testing.T) {
		err := db.RejectPatternChange(99999)
		if !errors.Is(err, ErrPatternChangeNotFound) {
			t.Errorf("Expected ErrPatternChangeNotFound, got %v", err)
		}
	})
}

func TestDeletePatternChange(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	t.Run("delete existing", func(t *testing.T) {
		pc := &PatternChange{
			Tier:       "high",
			Pattern:    "delete-test.txt",
			ChangeType: PatternChangeTypeAdd,
			Reason:     "Test delete",
		}
		if err := db.CreatePatternChange(pc); err != nil {
			t.Fatalf("CreatePatternChange failed: %v", err)
		}

		err := db.DeletePatternChange(pc.ID)
		if err != nil {
			t.Fatalf("DeletePatternChange failed: %v", err)
		}

		_, err = db.GetPatternChange(pc.ID)
		if !errors.Is(err, ErrPatternChangeNotFound) {
			t.Errorf("Expected ErrPatternChangeNotFound after delete, got %v", err)
		}
	})

	t.Run("delete non-existent", func(t *testing.T) {
		err := db.DeletePatternChange(99999)
		if !errors.Is(err, ErrPatternChangeNotFound) {
			t.Errorf("Expected ErrPatternChangeNotFound, got %v", err)
		}
	})

	t.Run("delete already deleted", func(t *testing.T) {
		pc := &PatternChange{
			Tier:       "high",
			Pattern:    "delete-twice.txt",
			ChangeType: PatternChangeTypeAdd,
			Reason:     "Test double delete",
		}
		if err := db.CreatePatternChange(pc); err != nil {
			t.Fatalf("CreatePatternChange failed: %v", err)
		}

		// First delete
		if err := db.DeletePatternChange(pc.ID); err != nil {
			t.Fatalf("First DeletePatternChange failed: %v", err)
		}

		// Second delete should return not found
		err := db.DeletePatternChange(pc.ID)
		if !errors.Is(err, ErrPatternChangeNotFound) {
			t.Errorf("Expected ErrPatternChangeNotFound on second delete, got %v", err)
		}
	})
}

func TestCountPendingPatternChanges(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	t.Run("empty database", func(t *testing.T) {
		count, err := db.CountPendingPatternChanges()
		if err != nil {
			t.Fatalf("CountPendingPatternChanges failed: %v", err)
		}
		if count != 0 {
			t.Errorf("Expected 0, got %d", count)
		}
	})

	// Create mixed status pattern changes
	for i := 0; i < 3; i++ {
		pc := &PatternChange{
			Tier:       "dangerous",
			Pattern:    "count-pending" + string(rune('A'+i)),
			ChangeType: PatternChangeTypeAdd,
			Reason:     "Test",
			Status:     PatternChangeStatusPending,
		}
		if err := db.CreatePatternChange(pc); err != nil {
			t.Fatalf("CreatePatternChange failed: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		pc := &PatternChange{
			Tier:       "high",
			Pattern:    "count-approved" + string(rune('A'+i)),
			ChangeType: PatternChangeTypeAdd,
			Reason:     "Test",
			Status:     PatternChangeStatusApproved,
		}
		if err := db.CreatePatternChange(pc); err != nil {
			t.Fatalf("CreatePatternChange failed: %v", err)
		}
	}

	t.Run("with mixed statuses", func(t *testing.T) {
		count, err := db.CountPendingPatternChanges()
		if err != nil {
			t.Fatalf("CountPendingPatternChanges failed: %v", err)
		}
		if count != 3 {
			t.Errorf("Expected 3 pending, got %d", count)
		}
	})

	// Approve one pending
	changes, err := db.ListPendingPatternChanges()
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) > 0 {
		if err := db.ApprovePatternChange(changes[0].ID); err != nil {
			t.Fatalf("ApprovePatternChange failed: %v", err)
		}
	}

	t.Run("after approval", func(t *testing.T) {
		count, err := db.CountPendingPatternChanges()
		if err != nil {
			t.Fatalf("CountPendingPatternChanges failed: %v", err)
		}
		if count != 2 {
			t.Errorf("Expected 2 pending after approval, got %d", count)
		}
	})
}

func TestPatternChangeOrdering(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Create pattern changes with distinct creation times
	times := []time.Time{
		time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC),
		time.Date(2024, 1, 2, 10, 0, 0, 0, time.UTC),
		time.Date(2024, 1, 3, 10, 0, 0, 0, time.UTC),
	}

	for i, ts := range times {
		pc := &PatternChange{
			Tier:       "high",
			Pattern:    "order-" + string(rune('A'+i)),
			ChangeType: PatternChangeTypeAdd,
			Reason:     "Test",
			CreatedAt:  ts,
		}
		if err := db.CreatePatternChange(pc); err != nil {
			t.Fatalf("CreatePatternChange failed: %v", err)
		}
	}

	changes, err := db.ListAllPatternChanges()
	if err != nil {
		t.Fatalf("ListAllPatternChanges failed: %v", err)
	}

	// Verify descending order (newest first)
	for i := 1; i < len(changes); i++ {
		if changes[i-1].CreatedAt.Before(changes[i].CreatedAt) {
			t.Errorf("Expected descending order, but %v < %v at index %d",
				changes[i-1].CreatedAt, changes[i].CreatedAt, i)
		}
	}
}
