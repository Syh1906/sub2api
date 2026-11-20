//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTextRefundAtFourteenDays(t *testing.T) {
	paid := time.Date(2026, time.November, 1, 10, 30, 45, 0, time.UTC)
	deadline := paid.Add(14 * 24 * time.Hour)
	require.False(t, textRefundExpired(paid, deadline.Add(-time.Nanosecond)))
	require.False(t, textRefundExpired(paid, deadline))
	require.True(t, textRefundExpired(paid, deadline.Add(time.Nanosecond)))
}
