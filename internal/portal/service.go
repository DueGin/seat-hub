package portal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/DueGin/vm2api-portal/internal/api"
	"github.com/DueGin/vm2api-portal/internal/provision"
	"github.com/DueGin/vm2api-portal/internal/secret"
	"github.com/DueGin/vm2api-portal/internal/store"
	"github.com/DueGin/vm2api-portal/internal/vm2api"
)

type Service struct {
	db           *store.DB
	client       vm2api.Client
	box          *secret.Box
	provisioning *provision.Service
	mu           sync.Mutex
	Now          func() time.Time
}

var (
	_               api.KeyService = (*Service)(nil)
	ErrUserNotFound                = api.Err(errors.New("user not found"), 404, "user_not_found", "用户不存在")
)

func New(db *store.DB, client vm2api.Client, box *secret.Box, optional ...*provision.Service) *Service {
	service := &Service{db: db, client: client, box: box, Now: time.Now}
	if len(optional) > 0 {
		service.provisioning = optional[0]
	}
	if service.provisioning == nil {
		service.provisioning = provision.New(db.DB, client)
		service.provisioning.Now = service.now
	}
	if service.provisioning.EnsureUser == nil {
		service.provisioning.EnsureUser = service.ensureUser
	}
	return service
}

func (service *Service) now() time.Time {
	return service.Now().UTC()
}

func (service *Service) ensureUser(ctx context.Context, userID int64) (string, error) {
	var upstream sql.NullString
	err := service.db.QueryRowContext(ctx, `SELECT vm2api_user_id FROM users WHERE id=?`, userID).Scan(&upstream)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrUserNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read upstream user: %w", err)
	}
	if upstream.String != "" {
		return upstream.String, nil
	}
	password, err := secret.RandomToken(32)
	if err != nil {
		return "", fmt.Errorf("generate upstream password: %w", err)
	}
	encrypted, err := service.box.Seal(password)
	if err != nil {
		return "", fmt.Errorf("encrypt upstream password: %w", err)
	}
	username := fmt.Sprintf("portal_%d", userID)
	user, err := service.client.CreateUser(ctx, username, password)
	if err != nil {
		return "", fmt.Errorf("create upstream user: %w", err)
	}
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := service.db.ExecContext(saveCtx, `UPDATE users SET vm2api_user_id=?,vm2api_username=?,vm2api_password_enc=?,updated_at=? WHERE id=?`, user.ID, username, encrypted, store.FmtTime(service.now()), userID); err != nil {
		return "", fmt.Errorf("save upstream user: %w", err)
	}
	return user.ID, nil
}
