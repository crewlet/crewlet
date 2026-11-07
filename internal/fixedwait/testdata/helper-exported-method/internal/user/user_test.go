package user

import (
	"testing"

	"example.com/fixture/internal/midtest"
)

func TestSettles(t *testing.T) {
	midtest.Clock().Settle()
}
