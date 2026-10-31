// Package app wires config, storage, domain services, HTTP API, and the
// protocol gateway into one process.
package app

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/DueGin/vm2api-portal/internal/api"
	"github.com/DueGin/vm2api-portal/internal/auth"
	"github.com/DueGin/vm2api-portal/internal/catalog"
	"github.com/DueGin/vm2api-portal/internal/config"
	"github.com/DueGin/vm2api-portal/internal/gateway"
	"github.com/DueGin/vm2api-portal/internal/order"
	"github.com/DueGin/vm2api-portal/internal/payment"
	"github.com/DueGin/vm2api-portal/internal/secret"
	"github.com/DueGin/vm2api-portal/internal/selfcheck"
	"github.com/DueGin/vm2api-portal/internal/store"
	"github.com/DueGin/vm2api-portal/internal/vm2api"
	"github.com/DueGin/vm2api-portal/internal/webdist"
)

type App struct {
	Cfg    config.Config
	Log    *slog.Logger
	DB     *store.DB
	Box    *secret.Box
	VM     vm2api.Client
	Server *http.Server

	resolver *resolver
	gw       *gateway.Gateway
	authSvc  *auth.Service
}

func New(cfg config.Config, logger *slog.Logger) (*App, error) {
	box, err := secret.New(cfg.SecretKey)
	if err != nil {
		return nil, err
	}
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	users := store.NewUsers(db)
	sessions := store.NewSessions(db)
	settings := store.NewSettings(db)
	audit := store.NewAudit(db)

	authSvc := &auth.Service{Box: box, Sessions: sessions, Users: users, Secure: cfg.SecureCookies()}
	authAdv := &authAdapter{svc: authSvc, users: users, limiter: auth.NewLimiter(10, 5*time.Minute)}

	vmClient := vm2api.NewHTTPClient(cfg.VM2APIBaseURL, cfg.VM2APIMasterKey, nil)

	settingsAdv := &settingsAdapter{settings: settings, audit: audit}
	catalogSvc := catalog.New(db.DB, cachedVMHealth(vmClient))
	orderSvc := order.New(db.DB, catalogSvc, nil)
	paymentSvc, err := payment.NewService(db.DB, orderSvc, payment.ServiceOptions{
		Providers: buildProviders(cfg, settingsAdv),
		Dev:       cfg.Dev,
	})
	if err != nil {
		return nil, err
	}
	paymentAdv := &paymentAdapter{db: db.DB, marker: orderSvc, settings: settingsAdv, dev: cfg.Dev, callbackBase: callbackBase(cfg)}
	paymentAdv.svc = paymentSvc

	// Usage/provisioning services are wired by wiring.go; they are the only
	// parts that need the job engine.
	prov := newProvisioner(db, vmClient, box, orderSvc, settings, logger)
	prov.svc.SetChecker(selfcheck.NewEngine(vmClient, db.DB))
	orderSvc.SetFulfiller(prov)

	usersAdv := &usersAdapter{users: users, db: db.DB, client: vmClient}
	usersAdv.entitle = func(ctx context.Context, userID int64) (api.AdminUserDTO, error) {
		ent, err := prov.prov.Entitlements(ctx, userID)
		if err != nil {
			return api.AdminUserDTO{}, err
		}
		return api.AdminUserDTO{
			ConcurrencyTotal: ent.Concurrency.Total,
			HostedUsed:       ent.HostedSeats.Used,
			HostedTotal:      ent.HostedSeats.Total,
			RentalUsed:       ent.RentalAccounts.Used,
			RentalTotal:      ent.RentalAccounts.Total,
		}, nil
	}

	deps := api.Deps{
		Users:    usersAdv,
		Settings: settingsAdv,
		Audit:    audit,
		Auth:     authAdv,
		Catalog:  &catalogAdapter{svc: catalogSvc},
		Orders:   &orderAdapter{svc: orderSvc},
		Payments: paymentAdv,
		Accounts: prov,
		Usage:    prov,
		Keys:     &keyAdapter{inner: prov.keys},
		Admin:    prov,
		Site:     siteFunc(settings, cfg.PublicBaseURL),
		Dev:      cfg.Dev,
	}
	apiServer := &api.Server{Deps: deps, Log: logger, Secure: cfg.SecureCookies()}

	res := newResolver(db.DB, box, prov.Entitlement)

	mux := http.NewServeMux()
	mux.Handle("/api/", authSvc.Middleware(apiServer.Handler()))
	mux.Handle("/", authSvc.Middleware(webdist.Handler("")))

	// QueueTimeout is admin-editable (settings.gateway_queue_timeout_ms).
	// Read once at wiring time: the value is captured for the process lifetime,
	// so a settings change applies on restart.
	queueTimeout := time.Duration(0)
	if site, err := store.LoadSite(context.Background(), settings); err != nil {
		logger.Warn("gateway queue timeout unavailable, failing fast", "err", err)
	} else if site.GatewayQueueTimeoutMS > 0 {
		queueTimeout = time.Duration(site.GatewayQueueTimeoutMS) * time.Millisecond
	}

	gw := gateway.New(gateway.Config{
		Upstream:     mustURL(cfg.VM2APIBaseURL),
		Resolver:     res,
		MaxBodyBytes: int64(cfg.MaxBodyMB) << 20,
		QueueTimeout: queueTimeout,
	})

	prov.SetInflightSource(gw.Inflight)

	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1") {
			gw.ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})

	app := &App{Cfg: cfg, Log: logger, DB: db, Box: box, VM: vmClient, resolver: res, gw: gw, authSvc: authSvc}
	app.Server = &http.Server{
		Addr:              cfg.Listen,
		Handler:           recoverer(logger, root),
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	prov.Attach(app.Server)
	return app, nil
}

// BootstrapAdmin creates the configured administrator when no account exists.
func (a *App) BootstrapAdmin(ctx context.Context) error {
	if a.Cfg.AdminEmail == "" || a.Cfg.AdminPassword == "" {
		return nil
	}
	users := store.NewUsers(a.DB)
	if n, err := users.Count(ctx); err != nil || n > 0 {
		return err
	}
	hash, err := auth.HashPassword(a.Cfg.AdminPassword)
	if err != nil {
		return err
	}
	u, err := users.Create(ctx, a.Cfg.AdminEmail, hash, "admin")
	if err != nil {
		return err
	}
	a.Log.Info("bootstrap admin created", "email", u.Email)
	return nil
}

func (a *App) Start() error {
	a.Log.Info("portal listening", "addr", a.Cfg.Listen, "vm2api", a.Cfg.VM2APIBaseURL, "dev", a.Cfg.Dev)
	return a.Server.ListenAndServe()
}

func (a *App) Close(ctx context.Context) error {
	_ = a.Server.Shutdown(ctx)
	return a.DB.Close()
}

// cachedVMHealth adapts vm2api's per-VM view into catalog's health probe and
// caches it: the public product list asks about every pooled VM on each
// request, and a stale-but-recent answer is better than a blocking fan-out.
func cachedVMHealth(client vm2api.Client) catalog.HealthFunc {
	probe := vmHealthFunc(client.GetAccountUsage)
	type entry struct {
		healthy bool
		expires time.Time
	}
	var (
		mu    sync.Mutex
		cache = map[string]entry{}
	)
	return func(vmID string) (bool, bool) {
		now := time.Now()
		mu.Lock()
		if e, ok := cache[vmID]; ok && e.expires.After(now) {
			mu.Unlock()
			return e.healthy, true
		}
		mu.Unlock()
		healthy, fetched := probe(vmID)
		mu.Lock()
		if len(cache) > 5000 {
			cache = map[string]entry{}
		}
		cache[vmID] = entry{healthy: healthy, expires: now.Add(30 * time.Second)}
		mu.Unlock()
		return healthy, fetched
	}
}

// siteFunc exposes the storefront config. The public base URL always comes from
// configuration when set: deriving it from the request Host would let a caller
// poison what every user is told to point their client at.
func siteFunc(settings *store.Settings, publicBaseURL string) func(r *http.Request) api.SiteConfig {
	return func(r *http.Request) api.SiteConfig {
		site, err := store.LoadSite(r.Context(), settings)
		if err != nil {
			site = store.DefaultSite()
		}
		base := strings.TrimRight(publicBaseURL, "/")
		if base == "" {
			base = baseURLFor(r)
		}
		return api.SiteConfig{
			SiteName:         site.SiteName,
			BaseURL:          base,
			RegistrationOpen: site.RegistrationOpen,
		}
	}
}

func baseURLFor(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = strings.TrimSpace(strings.Split(proto, ",")[0])
	}
	return scheme + "://" + r.Host
}

func recoverer(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				logger.Error("panic", "path", r.URL.Path, "panic", rec)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
