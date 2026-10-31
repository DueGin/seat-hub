package payment

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

type Manual struct {
	instructions string
}

func NewManual(instructions string) *Manual {
	if strings.TrimSpace(instructions) == "" {
		instructions = "转账后请联系管理员确认收款。"
	}
	return &Manual{instructions: instructions}
}

func (m *Manual) Create(ctx context.Context, req CreateRequest) (Payment, error) {
	if err := ctx.Err(); err != nil {
		return Payment{}, fmt.Errorf("create manual payment: %w", err)
	}
	if err := validateCreate(req); err != nil {
		return Payment{}, err
	}
	return Payment{Type: "manual", Instructions: m.instructions}, nil
}

func (m *Manual) Notify(_ *http.Request) (Notice, error) {
	return Notice{}, ErrNotifyUnsupported
}

type Mock struct {
	dev bool
}

func NewMock(dev bool) (*Mock, error) {
	if !dev {
		return nil, ErrProviderUnavailable
	}
	return &Mock{dev: true}, nil
}

func (m *Mock) Create(ctx context.Context, req CreateRequest) (Payment, error) {
	if !m.dev {
		return Payment{}, ErrProviderUnavailable
	}
	if err := ctx.Err(); err != nil {
		return Payment{}, fmt.Errorf("create mock payment: %w", err)
	}
	if err := validateCreate(req); err != nil {
		return Payment{}, err
	}
	return Payment{Type: "mock", Instructions: "仅用于开发环境，请点击模拟支付。"}, nil
}

func (m *Mock) Notify(_ *http.Request) (Notice, error) {
	return Notice{}, ErrNotifyUnsupported
}
