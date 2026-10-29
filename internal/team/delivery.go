package team

import (
	"errors"
	"fmt"
)

// SetDeliveryStatus 接受或丢弃一份已完成的成果；执行状态保持 completed。
func (s *Service) SetDeliveryStatus(runID, status string, version uint64) (*Run, bool, error) {
	if status != DeliveryAccepted && status != DeliveryDiscarded {
		return nil, false, errors.New("交付状态无效")
	}
	run := s.store.Run(runID)
	if run == nil {
		return nil, false, fmt.Errorf("%w: run=%s", ErrNotFound, runID)
	}
	if run.DeliveryStatus == status {
		return run, true, nil
	}
	state := s.stateFor(runID)
	state.mu.Lock()
	defer state.mu.Unlock()
	err := s.commitRun(runID, func(fresh *Run) ([]*Event, bool) {
		if fresh.DeliveryStatus == status {
			return nil, false
		}
		if fresh.Status != RunCompleted || fresh.DeliveryStatus != DeliveryPendingReview {
			return nil, false
		}
		if fresh.Version != version {
			return nil, false
		}
		fresh.DeliveryStatus = status
		return []*Event{newEvent(runID, EventRunDeliveryUpdated, "user", map[string]any{
			"deliveryStatus": status,
		})}, true
	})
	if err != nil {
		return nil, false, err
	}
	fresh := s.store.Run(runID)
	if fresh == nil {
		return nil, false, fmt.Errorf("%w: run=%s", ErrNotFound, runID)
	}
	if fresh.DeliveryStatus == status {
		return fresh, false, nil
	}
	if fresh.Status != RunCompleted || fresh.DeliveryStatus != DeliveryPendingReview {
		return nil, false, fmt.Errorf("%w: 当前成果状态不允许接受或丢弃", ErrRunFinished)
	}
	if fresh.Version != version {
		return nil, false, fmt.Errorf("%w: run=%s 期望 version=%d 实际=%d", ErrVersionConflict, runID, version, fresh.Version)
	}
	return nil, false, errors.New("交付状态更新失败")
}
