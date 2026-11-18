package service

import (
	"context"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

type UserRequest struct {
	ID         int64          `json:"id"`
	Kind       OpsRequestKind `json:"kind"`
	CreatedAt  time.Time      `json:"created_at"`
	RequestID  string         `json:"request_id"`
	Model      string         `json:"model"`
	DurationMs *int           `json:"duration_ms"`
}

func (s *OpsService) ListUserRequests(ctx context.Context, userID int64, allowErrors bool, filter OpsRequestDetailFilter) ([]UserRequest, int64, error) {
	if userID <= 0 {
		return nil, 0, ErrInsufficientPerms
	}
	if !allowErrors {
		if filter.Kind == string(OpsRequestKindError) {
			return []UserRequest{}, 0, nil
		}
		filter.Kind = string(OpsRequestKindSuccess)
	}
	if s.opsRepo == nil {
		return nil, 0, infraerrors.ServiceUnavailable("OPS_REPO_UNAVAILABLE", "Request history unavailable")
	}
	filter.UserID = &userID
	filter.CustomerView = true
	rows, total, err := s.opsRepo.ListRequestDetails(ctx, &filter)
	if err != nil {
		return nil, 0, err
	}
	items := make([]UserRequest, 0, len(rows))
	for _, r := range rows {
		items = append(items, UserRequest{ID: r.RecordID, Kind: r.Kind, CreatedAt: r.CreatedAt, RequestID: r.RequestID, Model: r.Model, DurationMs: r.DurationMs})
	}
	return items, total, nil
}
