package service

import (
	"context"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/apikey"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/shopspring/decimal"
)

// ExpectedQuota is the limit the administrator saw when opening the form.
// Checking it under the row lock prevents a stale form or replay from adding twice.
type AdminAPIKeyAccess struct {
	Quota         *float64 `json:"quota"`
	QuotaIncrease *float64 `json:"quota_increase"`
	ExpectedQuota *float64 `json:"expected_quota"`
	Status        *string  `json:"status"`
}

func (in AdminAPIKeyAccess) HasAccessFields() bool {
	return in.Quota != nil || in.QuotaIncrease != nil || in.ExpectedQuota != nil || in.Status != nil
}

func (in AdminAPIKeyAccess) apply(key *APIKey) (APIKeyUpdateFields, error) {
	var fields APIKeyUpdateFields
	changingQuota := in.Quota != nil || in.QuotaIncrease != nil
	if (in.Quota != nil && in.QuotaIncrease != nil) || changingQuota != (in.ExpectedQuota != nil) {
		return fields, infraerrors.BadRequest("API_KEY_QUOTA_INPUT", "Provide one quota change and the current expected_quota")
	}
	for _, value := range []*float64{in.Quota, in.QuotaIncrease, in.ExpectedQuota} {
		if value != nil {
			if err := validateAPIKeyLimit(*value); err != nil {
				return fields, err
			}
			if decimal.NewFromFloat(*value).Exponent() < -8 || *value >= 1e12 {
				return fields, infraerrors.BadRequest("API_KEY_QUOTA_PRECISION", "Quota must fit decimal(20,8)")
			}
		}
	}
	if in.Status != nil && *in.Status != StatusActive && *in.Status != "inactive" {
		return fields, infraerrors.BadRequest("API_KEY_STATUS_INVALID", "Status must be active or inactive")
	}
	if changingQuota {
		if key.Quota != *in.ExpectedQuota {
			return fields, infraerrors.Conflict("API_KEY_QUOTA_CHANGED", "Quota changed; read the current key before submitting again")
		}
		if in.QuotaIncrease != nil {
			if key.Quota == 0 || *in.QuotaIncrease <= 0 {
				return fields, infraerrors.BadRequest("API_KEY_QUOTA_INCREASE_INVALID", "A positive increase requires a finite existing quota")
			}
			total := decimal.NewFromFloat(key.Quota).Add(decimal.NewFromFloat(*in.QuotaIncrease))
			quota := total.InexactFloat64()
			if quota >= 1e12 || !decimal.NewFromFloat(quota).Equal(total) {
				return fields, infraerrors.BadRequest("API_KEY_QUOTA_PRECISION", "Result exceeds supported quota precision")
			}
			key.Quota = quota
		} else {
			key.Quota = *in.Quota
		}
		fields.Quota = true
	}
	previous := key.Status
	if in.Status != nil {
		key.Status = *in.Status
	}
	switch key.Status {
	case StatusAPIKeyActive, StatusAPIKeyExpired, StatusAPIKeyQuotaExhausted:
		switch {
		case key.IsExpired():
			key.Status = StatusAPIKeyExpired
		case key.IsQuotaExhausted():
			key.Status = StatusAPIKeyQuotaExhausted
		default:
			key.Status = StatusActive
		}
	}
	fields.Status = previous != key.Status
	return fields, nil
}

func (s *adminServiceImpl) GetAdminAPIKey(ctx context.Context, keyID int64) (*APIKey, error) {
	return s.apiKeyRepo.GetByID(ctx, keyID)
}

func (s *adminServiceImpl) AdminUpdateAPIKeyAccess(ctx context.Context, keyID int64, input AdminAPIKeyAccess) (*APIKey, error) {
	tx, err := s.entClient.Tx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.APIKey.Query().Where(apikey.IDEQ(keyID), apikey.DeletedAtIsNil()).ForUpdate().OnlyID(ctx)
	if dbent.IsNotFound(err) {
		return nil, ErrAPIKeyNotFound
	}
	if err != nil {
		return nil, err
	}
	opCtx := dbent.NewTxContext(ctx, tx)
	key, err := s.apiKeyRepo.GetByID(opCtx, keyID)
	if err != nil {
		return nil, err
	}
	fields, err := input.apply(key)
	if err != nil {
		return nil, err
	}
	if err = s.apiKeyRepo.Update(opCtx, key, fields); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	if s.authCacheInvalidator != nil {
		s.authCacheInvalidator.InvalidateAuthCacheByKey(ctx, key.Key)
	}
	return key, nil
}
