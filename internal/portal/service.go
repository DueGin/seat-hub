package portal

import (
	"errors"
	"sync"
	"time"

	"github.com/DueGin/seat-hub/internal/api"
	"github.com/DueGin/seat-hub/internal/provision"
	"github.com/DueGin/seat-hub/internal/secret"
	"github.com/DueGin/seat-hub/internal/store"
	"github.com/DueGin/seat-hub/internal/vm2api"
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
	if service.provisioning.Box == nil {
		service.provisioning.Box = box
	}
	return service
}

func (service *Service) now() time.Time {
	return service.Now().UTC()
}
