package app

import "github.com/DueGin/seat-hub/internal/api"

func newMailAdapter(deps featureDeps) api.MailService { return deps.Mail }
