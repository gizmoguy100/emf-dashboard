package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"

	_ "github.com/jackc/pgx/v5/stdlib"
	"gopkg.in/yaml.v3"
)

const (
	cacheKeyFavourites = "emf_favourites"
	cacheKeyBar        = "bar_snapshot"

	defaultAPICacheTTL       = 15 * time.Minute
	defaultAPITimeout        = 5 * time.Second
	backgroundRetryCooldown  = 10 * time.Minute
	backgroundRetryAttempts  = 3
	backgroundRetryWait      = 15 * time.Second
	apiCacheDatabaseTimeout  = 2 * time.Second
	apiCacheStatusTimeFormat = "Mon 15:04"
)

type config struct {
	Addr              string
	OwnerName         string
	PublicBaseURL     string
	PublicHostname    string
	DashboardHostname string
	DatabaseURL       string
	FavouritesURL     string
	BarAPIBaseURL     string
	BarEnabled        bool
	BarPollEvery      time.Duration
	APICacheTTL       time.Duration
	EventTZ           string
	HTTPTimeout       time.Duration
}

type fileConfig struct {
	App struct {
		HTTPAddr       string `yaml:"http_addr"`
		PublicBaseURL  string `yaml:"public_base_url"`
		PublicHostname string `yaml:"public_hostname"`
		HTTPTimeout    string `yaml:"http_timeout"`
	} `yaml:"app"`
	Owner struct {
		DisplayName string `yaml:"display_name"`
		Timezone    string `yaml:"timezone"`
	} `yaml:"owner"`
	Event struct {
		Timezone string `yaml:"timezone"`
	} `yaml:"event"`
	Database struct {
		URL string `yaml:"url"`
	} `yaml:"database"`
	ExternalAPIs struct {
		EMF struct {
			BaseURL        string `yaml:"base_url"`
			Token          string `yaml:"token"`
			FavouritesURL  string `yaml:"favourites_url"`
			RequestTimeout string `yaml:"request_timeout"`
		} `yaml:"emf"`
		Bar struct {
			Enabled      bool   `yaml:"enabled"`
			BaseURL      string `yaml:"base_url"`
			PollInterval string `yaml:"poll_interval"`
		} `yaml:"bar"`
	} `yaml:"external_apis"`
	Domain struct {
		Hostname          string `yaml:"hostname"`
		Name              string `yaml:"name"`
		DashboardHostname string `yaml:"dashboard_hostname"`
	} `yaml:"domain"`
}

type favourite struct {
	ID               int          `json:"id"`
	Type             string       `json:"type"`
	Names            string       `json:"names"`
	Title            string       `json:"title"`
	ShortDescription string       `json:"short_description"`
	Link             string       `json:"link"`
	Occurrences      []occurrence `json:"occurrences"`
}

type occurrence struct {
	StartDate string `json:"start_date"`
	EndDate   string `json:"end_date"`
	Venue     string `json:"venue"`
	StartTime string `json:"start_time"`
	EndTime   string `json:"end_time"`
	MapLink   string `json:"map_link"`
}

type barSession struct {
	OpeningTime string `json:"opening_time"`
	ClosingTime string `json:"closing_time"`
}

type barSessionsResponse struct {
	Sessions []barSession `json:"sessions"`
}

type barProgressResponse struct {
	LicensedTimePct        string `json:"licensed_time_pct"`
	ExpectedConsumptionPct string `json:"expected_consumption_pct"`
	ActualConsumptionPct   string `json:"actual_consumption_pct"`
}

type barOnTapResponse struct {
	Ales   []barStockItem `json:"ales"`
	Kegs   []barStockItem `json:"kegs"`
	Ciders []barStockItem `json:"ciders"`
}

type barStockItem struct {
	StockType    barStockType `json:"stocktype"`
	Description  string       `json:"description"`
	RemainingPct string       `json:"remaining_pct"`
}

type barStockType struct {
	FullName            string         `json:"fullname"`
	Price               string         `json:"price"`
	ABV                 string         `json:"abv"`
	BaseUnitsRemaining  string         `json:"base_units_remaining"`
	StockUnitName       string         `json:"stock_unit_name"`
	StockUnitNamePlural string         `json:"stock_unit_name_plural"`
	StockLines          []barStockLine `json:"stocklines"`
}

type barStockLine struct {
	LocationDisplay string `json:"location_display"`
	Location        string `json:"location"`
}

type barDepartmentResponse struct {
	StockTypes []barStockType `json:"stocktypes"`
}

type barDrink struct {
	Name         string
	Meta         string
	RemainingPct string
}

type barVenueStatus struct {
	Name        string
	StateLabel  string
	TimingLabel string
	OnTapCount  int
}

type clubMateStock struct {
	Name      string
	Remaining string
	Location  string
}

type barStatus struct {
	Enabled        bool
	SourceLabel    string
	StateLabel     string
	NextSession    string
	ConsumptionPct string
	DrinkCount     int
	Drinks         []barDrink
	Bars           []barVenueStatus
	ClubMate       []clubMateStock
	CachedAtLabel  string
	Warnings       []string
}

type barSnapshot struct {
	FetchedAt time.Time
	Sessions  []barSession
	Progress  barProgressResponse
	MainTap   barOnTapResponse
	CybarTap  barOnTapResponse
	ClubMate  []barStockType
}

type apiCacheEntry struct {
	Key        string
	Payload    []byte
	FetchedAt  time.Time
	ExpiresAt  time.Time
	LastError  string
	ErrorUntil time.Time
}

type apiCacheStore struct {
	db     *sql.DB
	logger *slog.Logger
}

type cacheStatus struct {
	Name         string
	StateLabel   string
	FetchedLabel string
	ExpiresLabel string
	ErrorLabel   string
}

type scheduledItem struct {
	ID          int
	Type        string
	Title       string
	Names       string
	Description string
	Link        string
	Venue       string
	Start       time.Time
	End         time.Time
	StartLabel  string
	EndLabel    string
	DayLabel    string
	Countdown   string
	IsNext      bool
}

type pageData struct {
	OwnerName        string
	NowLabel         string
	TimezoneLabel    string
	Mode             string
	Signal           string
	SourceLabel      string
	LastUpdatedLabel string
	Next             scheduledItem
	HasNext          bool
	Schedule         []scheduledItem
	Unscheduled      []favourite
	FavouriteCount   int
	Bar              barStatus
	Cache            []cacheStatus
	Weather          weather
	Notes            []string
	Warnings         []string
}

type contactLink struct {
	Label string
	Value string
	Href  string
	Meta  string
}

type contactPageData struct {
	OwnerName    string
	NowLabel     string
	DashboardURL string
	Links        []contactLink
}

type weather struct {
	Now   string
	Rain  string
	Night string
}

type server struct {
	cfg       config
	logger    *slog.Logger
	client    *http.Client
	templates *template.Template
	apiCache  *apiCacheStore
	retryMu   sync.Mutex
	retrying  map[string]bool
}

func main() {
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")

	cfg := loadConfig()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	apiCache, err := openAPICache(context.Background(), logger, cfg.DatabaseURL)
	if err != nil {
		logger.Warn("api cache unavailable; continuing without persistent api cache", "error", err)
	}
	if apiCache != nil {
		defer apiCache.close()
	}

	tmpl := template.Must(template.ParseFiles(
		"web/templates/dashboard.html",
		"web/templates/contact.html",
	))
	srv := &server{
		cfg:       cfg,
		logger:    logger,
		client:    &http.Client{Timeout: cfg.HTTPTimeout},
		templates: tmpl,
		apiCache:  apiCache,
		retrying:  make(map[string]bool),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleHome)
	mux.HandleFunc("/dashboard", srv.handleDashboard)
	mux.HandleFunc("/healthz", srv.handleHealth)
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("web/static"))))

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	logger.Info("starting server", "addr", cfg.Addr)
	if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func loadConfig() config {
	cfg := config{
		Addr:              ":8080",
		OwnerName:         "Config.User",
		PublicBaseURL:     "http://localhost:8080",
		PublicHostname:    "localhost",
		DashboardHostname: "localhost",
		BarAPIBaseURL:     "https://emftill.assorted.org.uk",
		BarEnabled:        true,
		BarPollEvery:      10 * time.Minute,
		APICacheTTL:       defaultAPICacheTTL,
		EventTZ:           "Europe/London",
		HTTPTimeout:       defaultAPITimeout,
	}

	configFile := env("APP_CONFIG_FILE", "config/config.yaml")
	if err := applyConfigFile(configFile, &cfg); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("could not load config file", "path", configFile, "error", err)
	}

	return cfg
}

func applyConfigFile(path string, cfg *config) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var file fileConfig
	if err := yaml.Unmarshal(content, &file); err != nil {
		return err
	}

	applyString(&cfg.Addr, file.App.HTTPAddr)
	applyString(&cfg.OwnerName, file.Owner.DisplayName)
	applyString(&cfg.PublicBaseURL, file.App.PublicBaseURL)
	applyString(&cfg.PublicHostname, file.App.PublicHostname)
	applyString(&cfg.PublicHostname, file.Domain.Name)
	applyString(&cfg.PublicHostname, file.Domain.Hostname)
	applyString(&cfg.DashboardHostname, file.Domain.DashboardHostname)
	applyString(&cfg.DatabaseURL, file.Database.URL)
	applyString(&cfg.FavouritesURL, file.ExternalAPIs.EMF.FavouritesURL)
	applyString(&cfg.BarAPIBaseURL, file.ExternalAPIs.Bar.BaseURL)
	cfg.BarEnabled = file.ExternalAPIs.Bar.Enabled
	if file.ExternalAPIs.Bar.PollInterval != "" {
		interval, err := time.ParseDuration(file.ExternalAPIs.Bar.PollInterval)
		if err != nil {
			return err
		}
		cfg.BarPollEvery = interval
	}

	if file.Event.Timezone != "" {
		cfg.EventTZ = file.Event.Timezone
	} else {
		applyString(&cfg.EventTZ, file.Owner.Timezone)
	}

	if file.ExternalAPIs.EMF.RequestTimeout != "" {
		timeout, err := time.ParseDuration(file.ExternalAPIs.EMF.RequestTimeout)
		if err != nil {
			return err
		}
		cfg.HTTPTimeout = timeout
	}
	if file.App.HTTPTimeout != "" {
		timeout, err := time.ParseDuration(file.App.HTTPTimeout)
		if err != nil {
			return err
		}
		cfg.HTTPTimeout = timeout
	}
	if cfg.HTTPTimeout <= 0 || cfg.HTTPTimeout > defaultAPITimeout {
		cfg.HTTPTimeout = defaultAPITimeout
	}
	if cfg.BarPollEvery <= 0 || cfg.BarPollEvery > defaultAPICacheTTL {
		cfg.BarPollEvery = defaultAPICacheTTL
	}

	return nil
}

func applyString(target *string, value string) {
	if strings.TrimSpace(value) != "" {
		*target = value
	}
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func openAPICache(parent context.Context, logger *slog.Logger, databaseURL string) (*apiCacheStore, error) {
	if strings.TrimSpace(databaseURL) == "" {
		return nil, errors.New("database.url is empty")
	}

	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(parent, apiCacheDatabaseTimeout)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}

	cache := &apiCacheStore{db: db, logger: logger}
	if err := cache.ensureSchema(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return cache, nil
}

func (c *apiCacheStore) close() {
	if c != nil && c.db != nil {
		_ = c.db.Close()
	}
}

func (c *apiCacheStore) ensureSchema(ctx context.Context) error {
	_, err := c.db.ExecContext(ctx, `
		create table if not exists api_cache (
			key text primary key,
			payload jsonb,
			fetched_at timestamptz not null,
			expires_at timestamptz not null,
			last_error text not null default '',
			error_until timestamptz
		)
	`)
	if err != nil {
		return err
	}
	_, err = c.db.ExecContext(ctx, `alter table api_cache alter column payload drop not null`)
	return err
}

func (c *apiCacheStore) get(parent context.Context, key string) (apiCacheEntry, bool, error) {
	if c == nil {
		return apiCacheEntry{}, false, nil
	}

	ctx, cancel := context.WithTimeout(parent, apiCacheDatabaseTimeout)
	defer cancel()

	var entry apiCacheEntry
	var errorUntil sql.NullTime
	err := c.db.QueryRowContext(ctx, `
		select key, payload, fetched_at, expires_at, last_error, error_until
		from api_cache
		where key = $1
	`, key).Scan(&entry.Key, &entry.Payload, &entry.FetchedAt, &entry.ExpiresAt, &entry.LastError, &errorUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return apiCacheEntry{}, false, nil
	}
	if err != nil {
		return apiCacheEntry{}, false, err
	}
	if errorUntil.Valid {
		entry.ErrorUntil = errorUntil.Time
	}
	return entry, true, nil
}

func (c *apiCacheStore) put(parent context.Context, key string, payload []byte, fetchedAt time.Time, ttl time.Duration) error {
	if c == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(parent, apiCacheDatabaseTimeout)
	defer cancel()

	_, err := c.db.ExecContext(ctx, `
		insert into api_cache (key, payload, fetched_at, expires_at, last_error, error_until)
		values ($1, $2::jsonb, $3, $4, '', null)
		on conflict (key) do update set
			payload = excluded.payload,
			fetched_at = excluded.fetched_at,
			expires_at = excluded.expires_at,
			last_error = '',
			error_until = null
	`, key, string(payload), fetchedAt.UTC(), fetchedAt.Add(ttl).UTC())
	return err
}

func (c *apiCacheStore) recordFailure(parent context.Context, key string, err error, now time.Time) {
	if c == nil {
		return
	}

	ctx, cancel := context.WithTimeout(parent, apiCacheDatabaseTimeout)
	defer cancel()

	message := err.Error()
	if len(message) > 500 {
		message = message[:500]
	}
	if _, dbErr := c.db.ExecContext(ctx, `
		insert into api_cache (key, payload, fetched_at, expires_at, last_error, error_until)
		values ($1, null, $3, $3, $2, $4)
		on conflict (key) do update set
			last_error = excluded.last_error,
			error_until = excluded.error_until
	`, key, message, now.UTC(), now.Add(backgroundRetryCooldown).UTC()); dbErr != nil {
		c.logger.Warn("record api cache failure", "key", key, "error", dbErr)
	}
}

func (c *apiCacheStore) statuses(parent context.Context, loc *time.Location, now time.Time) []cacheStatus {
	statuses := []cacheStatus{
		{Name: "EMF favourites", StateLabel: "missing", FetchedLabel: "Never", ExpiresLabel: "--"},
		{Name: "Bar snapshot", StateLabel: "missing", FetchedLabel: "Never", ExpiresLabel: "--"},
	}
	if c == nil {
		for i := range statuses {
			statuses[i].StateLabel = "database off"
		}
		return statuses
	}

	ctx, cancel := context.WithTimeout(parent, apiCacheDatabaseTimeout)
	defer cancel()

	rows, err := c.db.QueryContext(ctx, `
		select key, fetched_at, expires_at, last_error, error_until
		from api_cache
		where key in ($1, $2)
	`, cacheKeyFavourites, cacheKeyBar)
	if err != nil {
		c.logger.Warn("load api cache statuses", "error", err)
		return statuses
	}
	defer rows.Close()

	byKey := map[string]*cacheStatus{
		cacheKeyFavourites: &statuses[0],
		cacheKeyBar:        &statuses[1],
	}
	for rows.Next() {
		var key, lastError string
		var fetchedAt, expiresAt time.Time
		var errorUntil sql.NullTime
		if err := rows.Scan(&key, &fetchedAt, &expiresAt, &lastError, &errorUntil); err != nil {
			c.logger.Warn("scan api cache status", "error", err)
			continue
		}
		status := byKey[key]
		if status == nil {
			continue
		}
		status.FetchedLabel = fetchedAt.In(loc).Format(apiCacheStatusTimeFormat)
		status.ExpiresLabel = expiresAt.In(loc).Format(apiCacheStatusTimeFormat)
		switch {
		case errorUntil.Valid && now.Before(errorUntil.Time):
			status.StateLabel = "backoff"
		case now.Before(expiresAt):
			status.StateLabel = "fresh"
		default:
			status.StateLabel = "stale"
		}
		if lastError != "" {
			status.ErrorLabel = "Last error: " + lastError
		}
	}
	return statuses
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *server) handleHome(w http.ResponseWriter, r *http.Request) {
	if cleanHostname(r.Host) == s.cfg.DashboardHostname {
		s.handleDashboard(w, r)
		return
	}
	s.handleContact(w, r)
}

func (s *server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	data := s.page(r.Context())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "dashboard.html", data); err != nil {
		s.logger.Error("render dashboard", "error", err)
	}
}

func (s *server) handleContact(w http.ResponseWriter, _ *http.Request) {
	data := s.contactPage()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "contact.html", data); err != nil {
		s.logger.Error("render contact", "error", err)
	}
}

func cleanHostname(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	if i := strings.LastIndex(host, ":"); i > -1 {
		return host[:i]
	}
	return host
}

func (s *server) contactPage() contactPageData {
	loc, err := time.LoadLocation(s.cfg.EventTZ)
	if err != nil {
		loc = time.Local
	}

	dashboardURL := "/dashboard"
	if s.cfg.DashboardHostname != "" && s.cfg.DashboardHostname != "localhost" {
		dashboardURL = "https://" + s.cfg.DashboardHostname
	}

	return contactPageData{
		OwnerName:    s.cfg.OwnerName,
		NowLabel:     time.Now().In(loc).Format("15:04 MST"),
		DashboardURL: dashboardURL,
		Links: []contactLink{
			{Label: "EMF Phone", Value: "9221", Href: "tel:9221", Meta: "camp phone system"},
			{Label: "Email", Value: "alex@ferriroli.com", Href: "mailto:alex@ferriroli.com", Meta: "best for async"},
			{Label: "LinkedIn", Value: "alex-f-51642417", Href: "https://www.linkedin.com/in/alex-f-51642417/", Meta: "professional channel"},
			{Label: "Telegram", Value: "@gizmoguy100", Href: "https://t.me/gizmoguy100", Meta: "fast ping"},
			{Label: "Signal", Value: "signal.me contact link", Href: "https://signal.me/#eu/cnBA--khFXE2AuaNKQoFCrXAEqLhqHqP_v59GAxEPSR5DUJqPJVHFr45_9KNUW9E", Meta: "secure chat"},
			{Label: "WhatsApp", Value: "QR contact link", Href: "https://wa.me/qr/DQ7PO4D2KZSBH1", Meta: "mobile chat"},
		},
	}
}

func (s *server) startBackgroundRetry(key string, ttl time.Duration, refresh func(context.Context) ([]byte, error)) {
	if s.apiCache == nil {
		return
	}

	s.retryMu.Lock()
	if s.retrying[key] {
		s.retryMu.Unlock()
		return
	}
	s.retrying[key] = true
	s.retryMu.Unlock()

	go func() {
		defer func() {
			s.retryMu.Lock()
			delete(s.retrying, key)
			s.retryMu.Unlock()
		}()

		var lastErr error
		for attempt := 1; attempt <= backgroundRetryAttempts; attempt++ {
			if attempt > 1 {
				time.Sleep(backgroundRetryWait)
			}
			ctx, cancel := context.WithTimeout(context.Background(), s.cfg.HTTPTimeout)
			payload, err := refresh(ctx)
			cancel()
			if err == nil {
				now := time.Now().UTC()
				if err := s.apiCache.put(context.Background(), key, payload, now, ttl); err != nil {
					s.logger.Warn("store background api cache refresh", "key", key, "error", err)
				}
				return
			}
			lastErr = err
			s.logger.Warn("background api refresh failed", "key", key, "attempt", attempt, "error", err)
		}
		if lastErr != nil {
			s.apiCache.recordFailure(context.Background(), key, lastErr, time.Now().UTC())
		}
	}()
}

func (s *server) retryingKey(key string) bool {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	return s.retrying[key]
}

func (s *server) page(ctx context.Context) pageData {
	loc, err := time.LoadLocation(s.cfg.EventTZ)
	if err != nil {
		loc = time.Local
	}

	now := time.Now().In(loc)
	faves, source, warnings := s.loadFavourites(ctx)
	schedule, unscheduled := buildSchedule(faves, loc, now)
	bar := s.loadBarStatus(ctx, loc, now)

	data := pageData{
		OwnerName:        s.cfg.OwnerName,
		NowLabel:         now.Format("15:04 MST"),
		TimezoneLabel:    loc.String() + " // " + now.Format("MST"),
		Mode:             modeLabel(now),
		Signal:           signalLabel(s.cfg.FavouritesURL, warnings),
		SourceLabel:      source,
		LastUpdatedLabel: now.Format("Mon 15:04"),
		Schedule:         firstN(schedule, 6),
		Unscheduled:      firstNUnscheduled(unscheduled, 5),
		FavouriteCount:   len(faves),
		Bar:              bar,
		Cache:            s.cacheStatuses(ctx, loc, now),
		Weather: weather{
			Now:   "Check",
			Rain:  "Soon",
			Night: "Hoodie",
		},
		Notes: []string{
			"Keep Telegram reminders boring and reliable.",
			"Hydration and battery prompts matter more than novelty.",
			"Cache API responses before event connectivity gets weird.",
		},
		Warnings: warnings,
	}

	if len(schedule) > 0 {
		data.HasNext = true
		data.Next = schedule[0]
		data.Next.IsNext = true
		data.Schedule[0].IsNext = true
	}

	return data
}

func (s *server) cacheStatuses(ctx context.Context, loc *time.Location, now time.Time) []cacheStatus {
	return s.apiCache.statuses(ctx, loc, now)
}

func (s *server) loadFavourites(ctx context.Context) ([]favourite, string, []string) {
	if strings.TrimSpace(s.cfg.FavouritesURL) == "" {
		return sampleFavourites(), "sample data", []string{"Set external_apis.emf.favourites_url in config/config.yaml to render your real favourites."}
	}

	now := time.Now().UTC()
	entry, ok, err := s.apiCache.get(ctx, cacheKeyFavourites)
	if err != nil {
		s.logger.Warn("load favourites cache", "error", err)
	}
	if ok && now.Before(entry.ExpiresAt) {
		faves, err := decodeFavourites(entry.Payload)
		if err == nil {
			return faves, "cached favourites", nil
		}
		s.logger.Warn("decode cached favourites", "error", err)
	}
	if ok && (s.retryingKey(cacheKeyFavourites) || (!entry.ErrorUntil.IsZero() && now.Before(entry.ErrorUntil))) {
		faves, err := decodeFavourites(entry.Payload)
		if err == nil {
			return faves, "stale favourites", []string{"Using cached favourites while API refresh is paused after recent errors."}
		}
		return sampleFavourites(), "sample data", []string{"Favourites API refresh is paused after recent errors; showing sample data."}
	}
	if s.retryingKey(cacheKeyFavourites) {
		return sampleFavourites(), "sample data", []string{"Favourites API refresh is already retrying; showing sample data."}
	}

	faves, payload, err := s.fetchFavourites(ctx)
	if err == nil {
		if err := s.apiCache.put(ctx, cacheKeyFavourites, payload, now, s.cfg.APICacheTTL); err != nil {
			s.logger.Warn("store favourites cache", "error", err)
		}
		return faves, "live favourites", nil
	}

	s.logger.Warn("refresh favourites", "error", err)
	if ok {
		s.startBackgroundRetry(cacheKeyFavourites, s.cfg.APICacheTTL, func(retryCtx context.Context) ([]byte, error) {
			_, payload, err := s.fetchFavourites(retryCtx)
			return payload, err
		})
		faves, decodeErr := decodeFavourites(entry.Payload)
		if decodeErr == nil {
			return faves, "stale favourites", []string{"Could not refresh favourites; using cached data."}
		}
	}
	s.startBackgroundRetry(cacheKeyFavourites, s.cfg.APICacheTTL, func(retryCtx context.Context) ([]byte, error) {
		_, payload, err := s.fetchFavourites(retryCtx)
		return payload, err
	})
	return sampleFavourites(), "sample data", []string{"Could not fetch favourites; showing sample data."}
}

func (s *server) fetchFavourites(parent context.Context) ([]favourite, []byte, error) {
	ctx, cancel := context.WithTimeout(parent, s.cfg.HTTPTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.FavouritesURL, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "emf-dashboard/0.1")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, nil, errors.New(resp.Status)
	}

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	faves, err := decodeFavourites(payload)
	return faves, payload, err
}

func decodeFavourites(payload []byte) ([]favourite, error) {
	var faves []favourite
	if err := json.Unmarshal(payload, &faves); err != nil {
		return nil, err
	}
	return faves, nil
}

func (s *server) loadBarStatus(ctx context.Context, loc *time.Location, now time.Time) barStatus {
	status := barStatus{
		Enabled:     s.cfg.BarEnabled,
		SourceLabel: "bar API",
		StateLabel:  "unknown",
	}
	if !s.cfg.BarEnabled {
		status.SourceLabel = "disabled"
		status.StateLabel = "off"
		return status
	}
	if strings.TrimSpace(s.cfg.BarAPIBaseURL) == "" {
		status.SourceLabel = "unconfigured"
		status.StateLabel = "offline"
		status.Warnings = append(status.Warnings, "Set external_apis.bar.base_url to enable bar status.")
		return status
	}

	snapshot, warnings := s.cachedBarSnapshot(ctx, now)
	status.Warnings = append(status.Warnings, warnings...)
	status.ConsumptionPct = percentLabel(snapshot.Progress.ActualConsumptionPct)
	status.Drinks = firstNDrinks(barDrinks(snapshot.MainTap), 5)
	status.DrinkCount = countOnTap(snapshot.MainTap)
	status.ClubMate = clubMateStockLevels(snapshot.ClubMate)
	status.CachedAtLabel = snapshot.FetchedAt.In(loc).Format("15:04")

	state, timing := barSessionLabels(snapshot.Sessions, loc, now)
	status.StateLabel = state
	status.NextSession = timing
	status.Bars = []barVenueStatus{
		{Name: "Robot Arms", StateLabel: state, TimingLabel: timing, OnTapCount: countOnTap(snapshot.MainTap)},
		{Name: "Null Sector", StateLabel: state, TimingLabel: timing, OnTapCount: countOnTap(snapshot.CybarTap)},
	}

	if status.StateLabel == "unknown" && len(status.Warnings) > 0 {
		status.StateLabel = "degraded"
	}
	if status.ConsumptionPct == "" {
		status.ConsumptionPct = "--"
	}
	if status.NextSession == "" {
		status.NextSession = "No session listed"
	}

	return status
}

func (s *server) cachedBarSnapshot(ctx context.Context, now time.Time) (barSnapshot, []string) {
	now = now.UTC()
	entry, ok, err := s.apiCache.get(ctx, cacheKeyBar)
	if err != nil {
		s.logger.Warn("load bar cache", "error", err)
	}
	if ok && now.Before(entry.ExpiresAt) {
		snapshot, err := decodeBarSnapshot(entry.Payload)
		if err == nil {
			return snapshot, nil
		}
		s.logger.Warn("decode cached bar snapshot", "error", err)
	}
	if ok && (s.retryingKey(cacheKeyBar) || (!entry.ErrorUntil.IsZero() && now.Before(entry.ErrorUntil))) {
		snapshot, err := decodeBarSnapshot(entry.Payload)
		if err == nil {
			return snapshot, []string{"Using cached bar data while API refresh is paused after recent errors."}
		}
		return barSnapshot{FetchedAt: now}, []string{"Bar API refresh is paused after recent errors."}
	}
	if s.retryingKey(cacheKeyBar) {
		return barSnapshot{FetchedAt: now}, []string{"Bar API refresh is already retrying."}
	}

	snapshot, payload, warnings, err := s.fetchBarSnapshot(ctx, now)
	if err == nil {
		if err := s.apiCache.put(ctx, cacheKeyBar, payload, now, s.cfg.BarPollEvery); err != nil {
			s.logger.Warn("store bar cache", "error", err)
		}
		return snapshot, warnings
	}

	s.logger.Warn("refresh bar snapshot", "error", err)
	s.startBackgroundRetry(cacheKeyBar, s.cfg.BarPollEvery, func(retryCtx context.Context) ([]byte, error) {
		_, payload, _, err := s.fetchBarSnapshot(retryCtx, time.Now().UTC())
		return payload, err
	})
	if ok {
		cached, decodeErr := decodeBarSnapshot(entry.Payload)
		if decodeErr == nil {
			return cached, append(warnings, "Could not refresh bar data; using cached data.")
		}
	}
	if snapshot.FetchedAt.IsZero() {
		snapshot.FetchedAt = now
	}
	return snapshot, warnings
}

func (s *server) fetchBarSnapshot(parent context.Context, now time.Time) (barSnapshot, []byte, []string, error) {
	ctx, cancel := context.WithTimeout(parent, s.cfg.HTTPTimeout)
	defer cancel()

	snapshot := barSnapshot{FetchedAt: now}
	var warnings []string
	var errs []string

	sessions, err := fetchJSON[barSessionsResponse](ctx, s.client, s.cfg.BarAPIBaseURL, "/api/sessions.json")
	if err != nil {
		warnings = append(warnings, "Bar sessions unavailable.")
		errs = append(errs, "sessions: "+err.Error())
		s.logger.Warn("fetch bar sessions", "error", err)
	} else {
		snapshot.Sessions = sessions.Sessions
	}

	progress, err := fetchJSON[barProgressResponse](ctx, s.client, s.cfg.BarAPIBaseURL, "/api/progress.json")
	if err != nil {
		warnings = append(warnings, "Bar progress unavailable.")
		errs = append(errs, "progress: "+err.Error())
		s.logger.Warn("fetch bar progress", "error", err)
	} else {
		snapshot.Progress = progress
	}

	onTap, err := fetchJSON[barOnTapResponse](ctx, s.client, s.cfg.BarAPIBaseURL, "/api/on-tap.json")
	if err != nil {
		warnings = append(warnings, "Main bar on-tap list unavailable.")
		errs = append(errs, "on tap: "+err.Error())
		s.logger.Warn("fetch on tap", "error", err)
	} else {
		snapshot.MainTap = onTap
	}

	cybarTap, err := fetchJSON[barOnTapResponse](ctx, s.client, s.cfg.BarAPIBaseURL, "/api/cybar-on-tap.json")
	if err != nil {
		warnings = append(warnings, "Null Sector on-tap list unavailable.")
		errs = append(errs, "cybar on tap: "+err.Error())
		s.logger.Warn("fetch cybar on tap", "error", err)
	} else {
		snapshot.CybarTap = cybarTap
	}

	clubMate, err := fetchJSON[barDepartmentResponse](ctx, s.client, s.cfg.BarAPIBaseURL, "/api/department/75.json")
	if err != nil {
		warnings = append(warnings, "Club Mate stock unavailable.")
		errs = append(errs, "club mate: "+err.Error())
		s.logger.Warn("fetch club mate", "error", err)
	} else {
		snapshot.ClubMate = clubMate.StockTypes
	}

	payload, marshalErr := json.Marshal(snapshot)
	if marshalErr != nil {
		return snapshot, nil, warnings, marshalErr
	}
	if len(errs) > 0 {
		return snapshot, payload, warnings, errors.New(strings.Join(errs, "; "))
	}
	return snapshot, payload, warnings, nil
}

func decodeBarSnapshot(payload []byte) (barSnapshot, error) {
	var snapshot barSnapshot
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		return barSnapshot{}, err
	}
	return snapshot, nil
}

func fetchJSON[T any](ctx context.Context, client *http.Client, baseURL string, path string) (T, error) {
	var out T
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+path, nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "emf-dashboard/0.1")

	resp, err := client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return out, errors.New(resp.Status)
	}
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

func barSessionLabels(sessions []barSession, loc *time.Location, now time.Time) (string, string) {
	var nextOpen time.Time
	for _, session := range sessions {
		open, okOpen := parseAPITime(session.OpeningTime, loc)
		close, okClose := parseAPITime(session.ClosingTime, loc)
		if !okOpen || !okClose {
			continue
		}
		if (now.Equal(open) || now.After(open)) && now.Before(close) {
			return "open", "Open until " + close.Format("Mon 15:04")
		}
		if open.After(now) && (nextOpen.IsZero() || open.Before(nextOpen)) {
			nextOpen = open
		}
	}
	if !nextOpen.IsZero() {
		return "closed", "Open in " + durationLabel(nextOpen.Sub(now))
	}
	return "closed", "No future session"
}

func durationLabel(d time.Duration) string {
	if d < 0 {
		return "0 mins"
	}
	d = d.Round(time.Minute)
	hours := int(d.Hours())
	mins := int(d.Minutes()) % 60
	switch {
	case hours > 0 && mins > 0:
		return plural(hours, "hour") + " and " + plural(mins, "min")
	case hours > 0:
		return plural(hours, "hour")
	default:
		return plural(mins, "min")
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}

func parseAPITime(value string, loc *time.Location) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed.In(loc), true
	}
	if parsed, err := time.ParseInLocation("2006-01-02T15:04:05", value, loc); err == nil {
		return parsed.In(loc), true
	}
	return time.Time{}, false
}

func percentLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parts := strings.SplitN(value, ".", 2)
	return parts[0] + "%"
}

func barDrinks(onTap barOnTapResponse) []barDrink {
	items := append([]barStockItem{}, onTap.Ales...)
	items = append(items, onTap.Kegs...)
	items = append(items, onTap.Ciders...)

	drinks := make([]barDrink, 0, len(items))
	for _, item := range items {
		name := item.StockType.FullName
		if name == "" {
			name = item.Description
		}
		meta := "£" + item.StockType.Price
		if item.StockType.ABV != "" {
			meta += " // " + item.StockType.ABV + "%"
		}
		drinks = append(drinks, barDrink{
			Name:         name,
			Meta:         meta,
			RemainingPct: percentLabel(item.RemainingPct),
		})
	}
	return drinks
}

func firstNDrinks(items []barDrink, n int) []barDrink {
	if len(items) <= n {
		return items
	}
	return items[:n]
}

func countOnTap(onTap barOnTapResponse) int {
	return len(onTap.Ales) + len(onTap.Kegs) + len(onTap.Ciders)
}

func clubMateStockLevels(stockTypes []barStockType) []clubMateStock {
	levels := make([]clubMateStock, 0, len(stockTypes))
	for _, stock := range stockTypes {
		name := stock.FullName
		if name == "" {
			continue
		}
		unit := stock.StockUnitNamePlural
		if unit == "" {
			unit = stock.StockUnitName
		}
		remaining := strings.TrimSuffix(stock.BaseUnitsRemaining, ".0")
		if remaining == "" {
			remaining = "--"
		}
		if unit != "" {
			remaining += " " + unit
		}

		location := "Unknown"
		if len(stock.StockLines) > 0 {
			location = stock.StockLines[0].LocationDisplay
			if location == "" {
				location = stock.StockLines[0].Location
			}
		}

		levels = append(levels, clubMateStock{
			Name:      name,
			Remaining: remaining,
			Location:  location,
		})
	}
	return levels
}

func buildSchedule(faves []favourite, loc *time.Location, now time.Time) ([]scheduledItem, []favourite) {
	var schedule []scheduledItem
	var unscheduled []favourite

	for _, fave := range faves {
		if len(fave.Occurrences) == 0 {
			unscheduled = append(unscheduled, fave)
			continue
		}

		for _, occ := range fave.Occurrences {
			start, ok := parseEventTime(occ.StartDate, loc)
			if !ok {
				continue
			}
			end, _ := parseEventTime(occ.EndDate, loc)
			item := scheduledItem{
				ID:          fave.ID,
				Type:        fave.Type,
				Title:       fave.Title,
				Names:       fave.Names,
				Description: fave.ShortDescription,
				Link:        fave.Link,
				Venue:       occ.Venue,
				Start:       start,
				End:         end,
				StartLabel:  start.Format("15:04 MST"),
				EndLabel:    end.Format("15:04 MST"),
				DayLabel:    start.Format("Mon 2 Jan"),
				Countdown:   countdown(now, start),
			}
			schedule = append(schedule, item)
		}
	}

	sort.Slice(schedule, func(i, j int) bool {
		return schedule[i].Start.Before(schedule[j].Start)
	})

	upcoming := schedule[:0]
	for _, item := range schedule {
		if item.End.IsZero() || item.End.After(now) {
			upcoming = append(upcoming, item)
		}
	}
	if len(upcoming) > 0 {
		schedule = upcoming
	}

	sort.Slice(unscheduled, func(i, j int) bool {
		return strings.ToLower(unscheduled[i].Title) < strings.ToLower(unscheduled[j].Title)
	})

	return schedule, unscheduled
}

func parseEventTime(value string, loc *time.Location) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339} {
		if parsed, err := time.ParseInLocation(layout, value, loc); err == nil {
			return parsed.In(loc), true
		}
	}
	return time.Time{}, false
}

func countdown(now, start time.Time) string {
	d := start.Sub(now)
	if d < 0 {
		return "now"
	}
	d = d.Round(time.Minute)
	if d < time.Minute {
		return "T-0h 1m"
	}
	hours := int(d.Hours())
	mins := int(d.Minutes()) % 60
	if mins == 0 {
		return "T-" + plural(hours, "hour")
	}
	return "T-" + plural(hours, "hour") + " " + plural(mins, "min")
}

func modeLabel(now time.Time) string {
	if now.Hour() < 7 {
		return "Night Watch"
	}
	if now.Hour() < 12 {
		return "Morning Sync"
	}
	if now.Hour() < 18 {
		return "Event Day"
	}
	return "Evening Orbit"
}

func signalLabel(url string, warnings []string) string {
	if len(warnings) > 0 {
		return "Fallback"
	}
	if strings.TrimSpace(url) == "" {
		return "Offline"
	}
	return "Live"
}

func firstN(items []scheduledItem, n int) []scheduledItem {
	if len(items) <= n {
		return items
	}
	return items[:n]
}

func firstNUnscheduled(items []favourite, n int) []favourite {
	if len(items) <= n {
		return items
	}
	return items[:n]
}

func sampleFavourites() []favourite {
	return []favourite{
		{
			ID:               1,
			Type:             "talk",
			Names:            "Mission Control",
			Title:            "Keeping Tiny Computers Alive in a Field",
			ShortDescription: "Battery packs, waterproof boxes, optimistic cables, and the art of not missing the good bit.",
			Link:             "#",
			Occurrences: []occurrence{{
				StartDate: "2026-07-18 14:00:00",
				EndDate:   "2026-07-18 14:50:00",
				Venue:     "Stage A",
			}},
		},
		{
			ID:               2,
			Type:             "workshop",
			Names:            "Badge Crew",
			Title:            "Late Night Badge Surgery",
			ShortDescription: "Bring the cable you definitely packed somewhere sensible.",
			Link:             "#",
			Occurrences: []occurrence{{
				StartDate: "2026-07-18 21:30:00",
				EndDate:   "2026-07-18 23:00:00",
				Venue:     "Workshop 2",
			}},
		},
		{
			ID:               3,
			Type:             "talk",
			Names:            "Space Person",
			Title:            "Satellites, Mesh, and Questionable Antennas",
			ShortDescription: "A practical guide to believing in radio propagation just enough.",
			Link:             "#",
			Occurrences: []occurrence{{
				StartDate: "2026-07-19 15:30:00",
				EndDate:   "2026-07-19 16:20:00",
				Venue:     "Stage B",
			}},
		},
		{
			ID:               4,
			Type:             "talk",
			Names:            "Retro Track",
			Title:            "Retrocomputing After the Heat Death of the Universe",
			ShortDescription: "Unscheduled favourite placeholder.",
			Link:             "#",
		},
	}
}
