package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"net/url"
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
	cacheKeyHAStates   = "home_assistant_states"
	cacheKeyWeather    = "weather_current"

	defaultAPICacheTTL       = 30 * time.Minute
	defaultHACacheTTL        = 30 * time.Minute
	defaultWeatherCacheTTL   = 30 * time.Minute
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
	HAEnabled         bool
	HABaseURL         string
	HAAccessToken     string
	HAMetrics         []haMetricConfig
	HACacheEvery      time.Duration
	WeatherEnabled    bool
	WeatherBaseURL    string
	WeatherLocation   string
	WeatherLatitude   float64
	WeatherLongitude  float64
	WeatherCacheEvery time.Duration
	TelegramEnabled   bool
	TelegramBotToken  string
	TelegramChatIDs   map[int64]bool
	TelegramPollEvery time.Duration
	MiniBlogMaxLength int
	MiniBlogPostLimit int
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
	Telegram struct {
		Enabled        bool    `yaml:"enabled"`
		BotToken       string  `yaml:"bot_token"`
		AllowedChatIDs []int64 `yaml:"allowed_chat_ids"`
		PollInterval   string  `yaml:"poll_interval"`
		MiniBlog       struct {
			MaxLength      int `yaml:"max_length"`
			DashboardLimit int `yaml:"dashboard_limit"`
		} `yaml:"miniblog"`
	} `yaml:"telegram"`
	ExternalAPIs struct {
		EMF struct {
			BaseURL        string `yaml:"base_url"`
			Token          string `yaml:"token"`
			FavouritesURL  string `yaml:"favourites_url"`
			CacheInterval  string `yaml:"cache_interval"`
			RequestTimeout string `yaml:"request_timeout"`
		} `yaml:"emf"`
		Bar struct {
			Enabled      bool   `yaml:"enabled"`
			BaseURL      string `yaml:"base_url"`
			PollInterval string `yaml:"poll_interval"`
		} `yaml:"bar"`
		HomeAssistant struct {
			Enabled       bool   `yaml:"enabled"`
			BaseURL       string `yaml:"base_url"`
			AccessToken   string `yaml:"access_token"`
			CacheInterval string `yaml:"cache_interval"`
			Metrics       []struct {
				Label    string `yaml:"label"`
				EntityID string `yaml:"entity_id"`
				Mode     string `yaml:"mode"`
				Display  string `yaml:"display"`
				Unit     string `yaml:"unit"`
			} `yaml:"metrics"`
		} `yaml:"home_assistant"`
		Weather struct {
			Enabled       bool    `yaml:"enabled"`
			BaseURL       string  `yaml:"base_url"`
			Location      string  `yaml:"location"`
			Latitude      float64 `yaml:"latitude"`
			Longitude     float64 `yaml:"longitude"`
			CacheInterval string  `yaml:"cache_interval"`
		} `yaml:"weather"`
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

func (line *barStockLine) UnmarshalJSON(data []byte) error {
	var raw struct {
		LocationDisplay json.RawMessage `json:"location_display"`
		Location        string          `json:"location"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	line.Location = raw.Location
	line.LocationDisplay = decodeBarLocationDisplay(raw.LocationDisplay)
	return nil
}

func decodeBarLocationDisplay(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}

	var display string
	if err := json.Unmarshal(raw, &display); err == nil {
		return display
	}

	var location struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(raw, &location); err == nil {
		if location.Name != "" {
			return location.Name
		}
		return location.Slug
	}

	return ""
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

type haState struct {
	EntityID    string         `json:"entity_id"`
	State       string         `json:"state"`
	Attributes  map[string]any `json:"attributes"`
	LastUpdated string         `json:"last_updated"`
}

type haSnapshot struct {
	States      []haState         `json:"states"`
	DailyDeltas map[string]string `json:"daily_deltas,omitempty"`
	Warnings    []string          `json:"warnings,omitempty"`
}

type haVital struct {
	Label           string
	Value           string
	Meta            string
	HasProgress     bool
	ProgressPercent int
}

type haMetricConfig struct {
	Label    string
	EntityID string
	Mode     string
	Display  string
	Unit     string
}

type haVitals struct {
	Enabled       bool
	StateLabel    string
	CachedAtLabel string
	Items         []haVital
	Warnings      []string
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
	HAVitals         haVitals
	Bar              barStatus
	Cache            []cacheStatus
	Weather          weather
	MiniBlog         miniBlog
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
	Enabled       bool
	Location      string
	HeaderLabel   string
	Now           string
	Rain          string
	Night         string
	StateLabel    string
	CachedAtLabel string
	Warning       string
}

type miniBlogPost struct {
	ID        int64
	Body      string
	CreatedAt time.Time
	TimeLabel string
}

type miniBlog struct {
	Enabled    bool
	StateLabel string
	Posts      []miniBlogPost
	Warning    string
}

type openMeteoCurrentResponse struct {
	CurrentUnits struct {
		Temperature string `json:"temperature_2m"`
		Precip      string `json:"precipitation"`
		WeatherCode string `json:"weather_code"`
	} `json:"current_units"`
	Current struct {
		Time        string  `json:"time"`
		Temperature float64 `json:"temperature_2m"`
		Precip      float64 `json:"precipitation"`
		WeatherCode int     `json:"weather_code"`
	} `json:"current"`
}

type telegramUpdateResponse struct {
	OK          bool             `json:"ok"`
	Description string           `json:"description"`
	Result      []telegramUpdate `json:"result"`
}

type telegramAPIResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
}

type telegramUpdate struct {
	UpdateID      int64                  `json:"update_id"`
	Message       *telegramMessage       `json:"message"`
	CallbackQuery *telegramCallbackQuery `json:"callback_query"`
}

type telegramCallbackQuery struct {
	ID      string           `json:"id"`
	From    telegramUser     `json:"from"`
	Message *telegramMessage `json:"message"`
	Data    string           `json:"data"`
}

type telegramMessage struct {
	MessageID int64        `json:"message_id"`
	From      telegramUser `json:"from"`
	Chat      telegramChat `json:"chat"`
	Text      string       `json:"text"`
}

type telegramUser struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}

type telegramChat struct {
	ID int64 `json:"id"`
}

type telegramSendMessageRequest struct {
	ChatID      int64                `json:"chat_id"`
	Text        string               `json:"text"`
	ReplyMarkup telegramInlineMarkup `json:"reply_markup,omitempty"`
}

type telegramInlineMarkup struct {
	InlineKeyboard [][]telegramInlineButton `json:"inline_keyboard,omitempty"`
}

type telegramInlineButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
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

	srv.startTelegramBot(context.Background())

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
		BarPollEvery:      defaultAPICacheTTL,
		HACacheEvery:      defaultHACacheTTL,
		WeatherBaseURL:    "https://api.open-meteo.com",
		WeatherLocation:   "Eastnor Deer Park",
		WeatherLatitude:   52.0367,
		WeatherLongitude:  -2.3918,
		WeatherCacheEvery: defaultWeatherCacheTTL,
		TelegramChatIDs:   make(map[int64]bool),
		TelegramPollEvery: 2 * time.Second,
		MiniBlogMaxLength: 140,
		MiniBlogPostLimit: 8,
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
	cfg.TelegramEnabled = file.Telegram.Enabled
	applyString(&cfg.TelegramBotToken, file.Telegram.BotToken)
	cfg.TelegramChatIDs = make(map[int64]bool)
	for _, chatID := range file.Telegram.AllowedChatIDs {
		if chatID != 0 {
			cfg.TelegramChatIDs[chatID] = true
		}
	}
	if file.Telegram.PollInterval != "" {
		interval, err := time.ParseDuration(file.Telegram.PollInterval)
		if err != nil {
			return err
		}
		cfg.TelegramPollEvery = interval
	}
	if file.Telegram.MiniBlog.MaxLength > 0 {
		cfg.MiniBlogMaxLength = file.Telegram.MiniBlog.MaxLength
	}
	if file.Telegram.MiniBlog.DashboardLimit > 0 {
		cfg.MiniBlogPostLimit = file.Telegram.MiniBlog.DashboardLimit
	}
	applyString(&cfg.FavouritesURL, file.ExternalAPIs.EMF.FavouritesURL)
	applyString(&cfg.BarAPIBaseURL, file.ExternalAPIs.Bar.BaseURL)
	cfg.BarEnabled = file.ExternalAPIs.Bar.Enabled
	cfg.HAEnabled = file.ExternalAPIs.HomeAssistant.Enabled
	applyString(&cfg.HABaseURL, file.ExternalAPIs.HomeAssistant.BaseURL)
	applyString(&cfg.HAAccessToken, file.ExternalAPIs.HomeAssistant.AccessToken)
	cfg.HAMetrics = cfg.HAMetrics[:0]
	for _, metric := range file.ExternalAPIs.HomeAssistant.Metrics {
		if strings.TrimSpace(metric.Label) == "" || strings.TrimSpace(metric.EntityID) == "" {
			continue
		}
		cfg.HAMetrics = append(cfg.HAMetrics, haMetricConfig{
			Label:    metric.Label,
			EntityID: metric.EntityID,
			Mode:     metric.Mode,
			Display:  metric.Display,
			Unit:     metric.Unit,
		})
	}
	if file.ExternalAPIs.Bar.PollInterval != "" {
		interval, err := time.ParseDuration(file.ExternalAPIs.Bar.PollInterval)
		if err != nil {
			return err
		}
		cfg.BarPollEvery = interval
	}
	if file.ExternalAPIs.HomeAssistant.CacheInterval != "" {
		interval, err := time.ParseDuration(file.ExternalAPIs.HomeAssistant.CacheInterval)
		if err != nil {
			return err
		}
		cfg.HACacheEvery = interval
	}
	cfg.WeatherEnabled = file.ExternalAPIs.Weather.Enabled
	applyString(&cfg.WeatherBaseURL, file.ExternalAPIs.Weather.BaseURL)
	applyString(&cfg.WeatherLocation, file.ExternalAPIs.Weather.Location)
	if file.ExternalAPIs.Weather.Latitude != 0 {
		cfg.WeatherLatitude = file.ExternalAPIs.Weather.Latitude
	}
	if file.ExternalAPIs.Weather.Longitude != 0 {
		cfg.WeatherLongitude = file.ExternalAPIs.Weather.Longitude
	}
	if file.ExternalAPIs.Weather.CacheInterval != "" {
		interval, err := time.ParseDuration(file.ExternalAPIs.Weather.CacheInterval)
		if err != nil {
			return err
		}
		cfg.WeatherCacheEvery = interval
	}

	if file.Event.Timezone != "" {
		cfg.EventTZ = file.Event.Timezone
	} else {
		applyString(&cfg.EventTZ, file.Owner.Timezone)
	}

	if file.ExternalAPIs.EMF.CacheInterval != "" {
		interval, err := time.ParseDuration(file.ExternalAPIs.EMF.CacheInterval)
		if err != nil {
			return err
		}
		cfg.APICacheTTL = interval
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
	if cfg.APICacheTTL <= 0 || cfg.APICacheTTL > 24*time.Hour {
		cfg.APICacheTTL = defaultAPICacheTTL
	}
	if cfg.BarPollEvery <= 0 || cfg.BarPollEvery > defaultAPICacheTTL {
		cfg.BarPollEvery = defaultAPICacheTTL
	}
	if cfg.HACacheEvery <= 0 || cfg.HACacheEvery > defaultHACacheTTL {
		cfg.HACacheEvery = defaultHACacheTTL
	}
	if cfg.WeatherCacheEvery <= 0 || cfg.WeatherCacheEvery > defaultAPICacheTTL {
		cfg.WeatherCacheEvery = defaultWeatherCacheTTL
	}
	if cfg.TelegramPollEvery <= 0 || cfg.TelegramPollEvery > 30*time.Second {
		cfg.TelegramPollEvery = 2 * time.Second
	}
	if cfg.MiniBlogMaxLength <= 0 || cfg.MiniBlogMaxLength > 1000 {
		cfg.MiniBlogMaxLength = 140
	}
	if cfg.MiniBlogPostLimit <= 0 || cfg.MiniBlogPostLimit > 50 {
		cfg.MiniBlogPostLimit = 8
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
	if err != nil {
		return err
	}
	_, err = c.db.ExecContext(ctx, `
		create table if not exists miniblog_posts (
			id bigserial primary key,
			body text not null,
			chat_id bigint not null,
			telegram_message_id bigint,
			created_at timestamptz not null default now()
		)
	`)
	if err != nil {
		return err
	}
	_, err = c.db.ExecContext(ctx, `
		create table if not exists bot_state (
			key text primary key,
			value text not null
		)
	`)
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
		{Name: "Home Assistant", StateLabel: "missing", FetchedLabel: "Never", ExpiresLabel: "--"},
		{Name: "Weather", StateLabel: "missing", FetchedLabel: "Never", ExpiresLabel: "--"},
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
		where key in ($1, $2, $3, $4)
	`, cacheKeyFavourites, cacheKeyBar, cacheKeyHAStates, cacheKeyWeather)
	if err != nil {
		c.logger.Warn("load api cache statuses", "error", err)
		return statuses
	}
	defer rows.Close()

	byKey := map[string]*cacheStatus{
		cacheKeyFavourites: &statuses[0],
		cacheKeyBar:        &statuses[1],
		cacheKeyHAStates:   &statuses[2],
		cacheKeyWeather:    &statuses[3],
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

func (c *apiCacheStore) insertMiniBlogPost(parent context.Context, body string, chatID int64, messageID int64) error {
	if c == nil {
		return errors.New("database unavailable")
	}
	ctx, cancel := context.WithTimeout(parent, apiCacheDatabaseTimeout)
	defer cancel()

	_, err := c.db.ExecContext(ctx, `
		insert into miniblog_posts (body, chat_id, telegram_message_id)
		values ($1, $2, $3)
	`, body, chatID, messageID)
	return err
}

func (c *apiCacheStore) deleteLatestMiniBlogPost(parent context.Context, chatID int64) (miniBlogPost, bool, error) {
	if c == nil {
		return miniBlogPost{}, false, errors.New("database unavailable")
	}
	ctx, cancel := context.WithTimeout(parent, apiCacheDatabaseTimeout)
	defer cancel()

	var post miniBlogPost
	err := c.db.QueryRowContext(ctx, `
		delete from miniblog_posts
		where id = (
			select id from miniblog_posts
			where chat_id = $1
			order by created_at desc, id desc
			limit 1
		)
		returning id, body, created_at
	`, chatID).Scan(&post.ID, &post.Body, &post.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return miniBlogPost{}, false, nil
	}
	if err != nil {
		return miniBlogPost{}, false, err
	}
	return post, true, nil
}

func (c *apiCacheStore) miniBlogPosts(parent context.Context, loc *time.Location, limit int) ([]miniBlogPost, error) {
	if c == nil {
		return nil, errors.New("database unavailable")
	}
	if limit <= 0 {
		limit = 8
	}
	ctx, cancel := context.WithTimeout(parent, apiCacheDatabaseTimeout)
	defer cancel()

	rows, err := c.db.QueryContext(ctx, `
		select id, body, created_at
		from miniblog_posts
		order by created_at desc, id desc
		limit $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var posts []miniBlogPost
	for rows.Next() {
		var post miniBlogPost
		if err := rows.Scan(&post.ID, &post.Body, &post.CreatedAt); err != nil {
			return nil, err
		}
		post.TimeLabel = post.CreatedAt.In(loc).Format("Mon 15:04")
		posts = append(posts, post)
	}
	return posts, rows.Err()
}

func (c *apiCacheStore) botState(parent context.Context, key string) (string, bool, error) {
	if c == nil {
		return "", false, errors.New("database unavailable")
	}
	ctx, cancel := context.WithTimeout(parent, apiCacheDatabaseTimeout)
	defer cancel()

	var value string
	err := c.db.QueryRowContext(ctx, `select value from bot_state where key = $1`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

func (c *apiCacheStore) setBotState(parent context.Context, key string, value string) error {
	if c == nil {
		return errors.New("database unavailable")
	}
	ctx, cancel := context.WithTimeout(parent, apiCacheDatabaseTimeout)
	defer cancel()

	_, err := c.db.ExecContext(ctx, `
		insert into bot_state (key, value)
		values ($1, $2)
		on conflict (key) do update set value = excluded.value
	`, key, value)
	return err
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

func (s *server) startTelegramBot(parent context.Context) {
	if !s.cfg.TelegramEnabled {
		return
	}
	if strings.TrimSpace(s.cfg.TelegramBotToken) == "" {
		s.logger.Warn("telegram bot enabled without bot token")
		return
	}
	if len(s.cfg.TelegramChatIDs) == 0 {
		s.logger.Warn("telegram bot enabled without allowed chat ids")
		return
	}
	if s.apiCache == nil {
		s.logger.Warn("telegram bot enabled without database")
		return
	}

	go s.telegramBotLoop(parent)
}

func (s *server) telegramBotLoop(parent context.Context) {
	offset := int64(0)
	if value, ok, err := s.apiCache.botState(parent, "telegram_update_offset"); err == nil && ok {
		if parsed, parseErr := strconv.ParseInt(value, 10, 64); parseErr == nil {
			offset = parsed
		}
	} else if err != nil {
		s.logger.Warn("load telegram offset", "error", err)
	}

	s.logger.Info("telegram miniblog bot started")
	for {
		updates, err := s.telegramGetUpdates(parent, offset)
		if err != nil {
			s.logger.Warn("telegram get updates", "error", err)
			time.Sleep(s.cfg.TelegramPollEvery)
			continue
		}
		for _, update := range updates {
			if update.UpdateID >= offset {
				offset = update.UpdateID + 1
			}
			s.handleTelegramUpdate(parent, update)
		}
		if len(updates) > 0 {
			if err := s.apiCache.setBotState(parent, "telegram_update_offset", strconv.FormatInt(offset, 10)); err != nil {
				s.logger.Warn("store telegram offset", "error", err)
			}
		}
	}
}

func (s *server) telegramGetUpdates(parent context.Context, offset int64) ([]telegramUpdate, error) {
	ctx, cancel := context.WithTimeout(parent, 35*time.Second)
	defer cancel()

	request := map[string]any{
		"offset":          offset,
		"timeout":         25,
		"allowed_updates": []string{"message", "callback_query"},
	}
	var response telegramUpdateResponse
	if err := s.telegramRequest(ctx, "getUpdates", request, &response); err != nil {
		return nil, err
	}
	if !response.OK {
		return nil, errors.New(response.Description)
	}
	return response.Result, nil
}

func (s *server) handleTelegramUpdate(ctx context.Context, update telegramUpdate) {
	if update.Message != nil {
		s.handleTelegramMessage(ctx, *update.Message)
		return
	}
	if update.CallbackQuery != nil {
		s.handleTelegramCallback(ctx, *update.CallbackQuery)
	}
}

func (s *server) handleTelegramMessage(ctx context.Context, message telegramMessage) {
	if !s.telegramAllowed(message.From.ID, message.Chat.ID) {
		return
	}

	text := strings.TrimSpace(message.Text)
	if text == "" {
		return
	}
	if strings.HasPrefix(text, "/") {
		s.handleTelegramCommand(ctx, message.Chat.ID, text)
		return
	}
	s.createMiniBlogPostFromTelegram(ctx, message.Chat.ID, message.MessageID, text)
}

func (s *server) handleTelegramCommand(ctx context.Context, chatID int64, command string) {
	command = strings.ToLower(strings.Fields(command)[0])
	switch command {
	case "/start", "/help":
		s.telegramSendMessage(ctx, chatID, "MiniBlog is ready. Send a short text post, or use the buttons below.", telegramMiniBlogKeyboard())
	case "/new", "/post":
		s.telegramSendMessage(ctx, chatID, "Send the MiniBlog text now. Keep it to "+strconv.Itoa(s.cfg.MiniBlogMaxLength)+" characters.", telegramMiniBlogKeyboard())
	case "/latest", "/recent":
		s.sendTelegramRecentPosts(ctx, chatID)
	case "/delete_latest":
		s.deleteLatestMiniBlogPostFromTelegram(ctx, chatID)
	default:
		s.telegramSendMessage(ctx, chatID, "Unknown command. Try /post, /latest, or /delete_latest.", telegramMiniBlogKeyboard())
	}
}

func (s *server) handleTelegramCallback(ctx context.Context, callback telegramCallbackQuery) {
	chatID := callback.From.ID
	if callback.Message != nil {
		chatID = callback.Message.Chat.ID
	}
	if !s.telegramAllowed(callback.From.ID, chatID) {
		return
	}
	_ = s.telegramAnswerCallback(ctx, callback.ID)

	switch callback.Data {
	case "new":
		s.telegramSendMessage(ctx, chatID, "Send the MiniBlog text now. Keep it to "+strconv.Itoa(s.cfg.MiniBlogMaxLength)+" characters.", telegramMiniBlogKeyboard())
	case "recent":
		s.sendTelegramRecentPosts(ctx, chatID)
	case "delete_latest":
		s.deleteLatestMiniBlogPostFromTelegram(ctx, chatID)
	case "help":
		s.telegramSendMessage(ctx, chatID, "MiniBlog posts are plain text only for now. Send a normal message to publish it.", telegramMiniBlogKeyboard())
	default:
		s.telegramSendMessage(ctx, chatID, "I do not know that button yet.", telegramMiniBlogKeyboard())
	}
}

func (s *server) createMiniBlogPostFromTelegram(ctx context.Context, chatID int64, messageID int64, text string) {
	length := len([]rune(text))
	if length > s.cfg.MiniBlogMaxLength {
		s.telegramSendMessage(ctx, chatID, "That post is "+strconv.Itoa(length)+" characters. Keep it to "+strconv.Itoa(s.cfg.MiniBlogMaxLength)+".", telegramMiniBlogKeyboard())
		return
	}
	if err := s.apiCache.insertMiniBlogPost(ctx, text, chatID, messageID); err != nil {
		s.logger.Warn("insert miniblog post", "error", err)
		s.telegramSendMessage(ctx, chatID, "Could not save that post. Try again in a moment.", telegramMiniBlogKeyboard())
		return
	}
	s.telegramSendMessage(ctx, chatID, "Posted to MiniBlog.", telegramMiniBlogKeyboard())
}

func (s *server) deleteLatestMiniBlogPostFromTelegram(ctx context.Context, chatID int64) {
	post, ok, err := s.apiCache.deleteLatestMiniBlogPost(ctx, chatID)
	if err != nil {
		s.logger.Warn("delete latest miniblog post", "error", err)
		s.telegramSendMessage(ctx, chatID, "Could not delete the latest post.", telegramMiniBlogKeyboard())
		return
	}
	if !ok {
		s.telegramSendMessage(ctx, chatID, "There are no MiniBlog posts to delete.", telegramMiniBlogKeyboard())
		return
	}
	s.telegramSendMessage(ctx, chatID, "Deleted latest post: "+post.Body, telegramMiniBlogKeyboard())
}

func (s *server) sendTelegramRecentPosts(ctx context.Context, chatID int64) {
	loc, err := time.LoadLocation(s.cfg.EventTZ)
	if err != nil {
		loc = time.Local
	}
	posts, err := s.apiCache.miniBlogPosts(ctx, loc, 5)
	if err != nil {
		s.logger.Warn("load recent miniblog posts", "error", err)
		s.telegramSendMessage(ctx, chatID, "Could not load recent posts.", telegramMiniBlogKeyboard())
		return
	}
	if len(posts) == 0 {
		s.telegramSendMessage(ctx, chatID, "No MiniBlog posts yet.", telegramMiniBlogKeyboard())
		return
	}
	var builder strings.Builder
	builder.WriteString("Recent MiniBlog posts:")
	for _, post := range posts {
		builder.WriteString("\n- ")
		builder.WriteString(post.TimeLabel)
		builder.WriteString(": ")
		builder.WriteString(post.Body)
	}
	s.telegramSendMessage(ctx, chatID, builder.String(), telegramMiniBlogKeyboard())
}

func (s *server) telegramAllowed(userID int64, chatID int64) bool {
	if userID != 0 && s.cfg.TelegramChatIDs[userID] {
		return true
	}
	return chatID != 0 && s.cfg.TelegramChatIDs[chatID]
}

func telegramMiniBlogKeyboard() telegramInlineMarkup {
	return telegramInlineMarkup{
		InlineKeyboard: [][]telegramInlineButton{
			{
				{Text: "New post", CallbackData: "new"},
				{Text: "Recent posts", CallbackData: "recent"},
			},
			{
				{Text: "Delete latest", CallbackData: "delete_latest"},
				{Text: "Help", CallbackData: "help"},
			},
		},
	}
}

func (s *server) telegramSendMessage(ctx context.Context, chatID int64, text string, keyboard telegramInlineMarkup) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.HTTPTimeout)
	defer cancel()

	request := telegramSendMessageRequest{
		ChatID:      chatID,
		Text:        text,
		ReplyMarkup: keyboard,
	}
	var response telegramAPIResponse
	if err := s.telegramRequest(ctx, "sendMessage", request, &response); err != nil {
		s.logger.Warn("telegram send message", "error", err)
		return
	}
	if !response.OK {
		s.logger.Warn("telegram send message rejected", "description", response.Description)
	}
}

func (s *server) telegramAnswerCallback(ctx context.Context, callbackID string) error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.HTTPTimeout)
	defer cancel()

	var response telegramAPIResponse
	err := s.telegramRequest(ctx, "answerCallbackQuery", map[string]string{"callback_query_id": callbackID}, &response)
	if err != nil {
		return err
	}
	if !response.OK {
		return errors.New(response.Description)
	}
	return nil
}

func (s *server) telegramRequest(ctx context.Context, method string, request any, response any) error {
	payload, err := json.Marshal(request)
	if err != nil {
		return err
	}
	endpoint := "https://api.telegram.org/bot" + s.cfg.TelegramBotToken + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "emf-dashboard/0.1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return s.sanitizeTelegramError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return errors.New(resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(response)
}

func (s *server) sanitizeTelegramError(err error) error {
	token := strings.TrimSpace(s.cfg.TelegramBotToken)
	if token == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), token, "[redacted]"))
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
	haVitals := s.loadHAVitals(ctx, loc, now)
	bar := s.loadBarStatus(ctx, loc, now)
	weather := s.loadWeather(ctx, loc, now)
	miniBlog := s.loadMiniBlog(ctx, loc)

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
		HAVitals:         haVitals,
		Bar:              bar,
		Cache:            s.cacheStatuses(ctx, loc, now),
		Weather:          weather,
		MiniBlog:         miniBlog,
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

func (s *server) loadMiniBlog(ctx context.Context, loc *time.Location) miniBlog {
	status := miniBlog{
		Enabled:    s.cfg.TelegramEnabled,
		StateLabel: "off",
	}
	if !s.cfg.TelegramEnabled {
		status.Warning = "Telegram MiniBlog is disabled."
		return status
	}
	if strings.TrimSpace(s.cfg.TelegramBotToken) == "" || len(s.cfg.TelegramChatIDs) == 0 {
		status.StateLabel = "unconfigured"
		if strings.TrimSpace(s.cfg.TelegramBotToken) == "" {
			status.Warning = "Set telegram.bot_token to enable MiniBlog posting."
		} else {
			status.Warning = "Set telegram.allowed_chat_ids to enable MiniBlog posting."
		}
		return status
	}

	posts, err := s.apiCache.miniBlogPosts(ctx, loc, s.cfg.MiniBlogPostLimit)
	if err != nil {
		s.logger.Warn("load miniblog posts", "error", err)
		status.StateLabel = "unavailable"
		status.Warning = "Could not load MiniBlog posts."
		return status
	}
	status.StateLabel = "live"
	status.Posts = posts
	return status
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

func (s *server) loadWeather(ctx context.Context, loc *time.Location, now time.Time) weather {
	status := weather{
		Enabled:     s.cfg.WeatherEnabled,
		Location:    s.cfg.WeatherLocation,
		HeaderLabel: "--",
		Now:         "--",
		Rain:        "--",
		Night:       "--",
		StateLabel:  "off",
	}
	if !s.cfg.WeatherEnabled {
		status.Warning = "Set external_apis.weather.enabled to true to show current temperature."
		return status
	}
	if strings.TrimSpace(s.cfg.WeatherBaseURL) == "" || s.cfg.WeatherLatitude == 0 || s.cfg.WeatherLongitude == 0 {
		status.StateLabel = "unconfigured"
		status.Warning = "Set external_apis.weather latitude and longitude."
		return status
	}

	now = now.UTC()
	entry, ok, err := s.apiCache.get(ctx, cacheKeyWeather)
	if err != nil {
		s.logger.Warn("load weather cache", "error", err)
	}
	if ok && now.Before(entry.ExpiresAt) {
		forecast, err := decodeWeather(entry.Payload)
		if err == nil {
			return weatherFromForecast(forecast, entry.FetchedAt, loc, s.cfg.WeatherLocation, "cached", "")
		}
		s.logger.Warn("decode cached weather", "error", err)
	}
	if ok && (s.retryingKey(cacheKeyWeather) || (!entry.ErrorUntil.IsZero() && now.Before(entry.ErrorUntil))) {
		forecast, err := decodeWeather(entry.Payload)
		if err == nil {
			return weatherFromForecast(forecast, entry.FetchedAt, loc, s.cfg.WeatherLocation, "stale", "Using cached weather while refresh is paused after recent errors.")
		}
		status.StateLabel = "backoff"
		status.Warning = "Weather refresh is paused after recent errors."
		return status
	}
	if s.retryingKey(cacheKeyWeather) {
		status.StateLabel = "retrying"
		status.Warning = "Weather refresh is already retrying."
		return status
	}

	forecast, payload, err := s.fetchWeather(ctx, loc)
	if err == nil {
		if err := s.apiCache.put(ctx, cacheKeyWeather, payload, now, s.cfg.WeatherCacheEvery); err != nil {
			s.logger.Warn("store weather cache", "error", err)
		}
		return weatherFromForecast(forecast, now, loc, s.cfg.WeatherLocation, "live", "")
	}

	s.logger.Warn("refresh weather", "error", err)
	s.startBackgroundRetry(cacheKeyWeather, s.cfg.WeatherCacheEvery, func(retryCtx context.Context) ([]byte, error) {
		_, payload, err := s.fetchWeather(retryCtx, loc)
		return payload, err
	})
	if ok {
		forecast, decodeErr := decodeWeather(entry.Payload)
		if decodeErr == nil {
			return weatherFromForecast(forecast, entry.FetchedAt, loc, s.cfg.WeatherLocation, "stale", "Could not refresh weather; using cached data.")
		}
	}
	status.StateLabel = "unavailable"
	status.Warning = "Could not fetch current weather."
	return status
}

func (s *server) fetchWeather(parent context.Context, loc *time.Location) (openMeteoCurrentResponse, []byte, error) {
	ctx, cancel := context.WithTimeout(parent, s.cfg.HTTPTimeout)
	defer cancel()

	endpoint, err := url.Parse(strings.TrimRight(s.cfg.WeatherBaseURL, "/") + "/v1/forecast")
	if err != nil {
		return openMeteoCurrentResponse{}, nil, err
	}
	query := endpoint.Query()
	query.Set("latitude", strconv.FormatFloat(s.cfg.WeatherLatitude, 'f', -1, 64))
	query.Set("longitude", strconv.FormatFloat(s.cfg.WeatherLongitude, 'f', -1, 64))
	query.Set("current", "temperature_2m,precipitation,weather_code")
	query.Set("timezone", loc.String())
	endpoint.RawQuery = query.Encode()

	payload, err := fetchRawJSON(ctx, s.client, endpoint.String())
	if err != nil {
		return openMeteoCurrentResponse{}, nil, err
	}
	forecast, err := decodeWeather(payload)
	return forecast, payload, err
}

func decodeWeather(payload []byte) (openMeteoCurrentResponse, error) {
	var forecast openMeteoCurrentResponse
	if err := json.Unmarshal(payload, &forecast); err != nil {
		return openMeteoCurrentResponse{}, err
	}
	return forecast, nil
}

func weatherFromForecast(forecast openMeteoCurrentResponse, fetchedAt time.Time, loc *time.Location, location string, source string, warning string) weather {
	unit := forecast.CurrentUnits.Temperature
	if unit == "" {
		unit = "°C"
	}
	temp := formatTemperature(forecast.Current.Temperature, unit)
	rainUnit := forecast.CurrentUnits.Precip
	if rainUnit == "" {
		rainUnit = "mm"
	}

	return weather{
		Enabled:       true,
		Location:      location,
		HeaderLabel:   temp,
		Now:           temp,
		Rain:          formatPrecipitation(forecast.Current.Precip, rainUnit),
		Night:         layerHint(forecast.Current.Temperature),
		StateLabel:    source,
		CachedAtLabel: fetchedAt.In(loc).Format("15:04"),
		Warning:       warning,
	}
}

func formatTemperature(temp float64, unit string) string {
	return strconv.FormatInt(int64(math.Round(temp)), 10) + unit
}

func formatPrecipitation(value float64, unit string) string {
	if value == 0 {
		return "Dry"
	}
	return formatHADelta(value) + " " + unit
}

func layerHint(temp float64) string {
	switch {
	case temp < 10:
		return "Coat"
	case temp < 16:
		return "Hoodie"
	case temp < 21:
		return "Light layer"
	default:
		return "T-shirt"
	}
}

func (s *server) loadHAVitals(ctx context.Context, loc *time.Location, now time.Time) haVitals {
	status := haVitals{
		Enabled:    s.cfg.HAEnabled,
		StateLabel: "off",
	}
	if !s.cfg.HAEnabled {
		return status
	}
	if strings.TrimSpace(s.cfg.HABaseURL) == "" || strings.TrimSpace(s.cfg.HAAccessToken) == "" {
		status.StateLabel = "unconfigured"
		status.Warnings = append(status.Warnings, "Set external_apis.home_assistant.base_url and access_token to show phone vitals.")
		return status
	}

	now = now.UTC()
	entry, ok, err := s.apiCache.get(ctx, cacheKeyHAStates)
	if err != nil {
		s.logger.Warn("load home assistant cache", "error", err)
	}
	if ok && now.Before(entry.ExpiresAt) {
		snapshot, err := decodeHASnapshot(entry.Payload)
		if err == nil {
			return s.haVitalsFromSnapshot(snapshot, entry.FetchedAt, loc, "cached")
		}
		s.logger.Warn("decode cached home assistant states", "error", err)
	}
	if ok && (s.retryingKey(cacheKeyHAStates) || (!entry.ErrorUntil.IsZero() && now.Before(entry.ErrorUntil))) {
		snapshot, err := decodeHASnapshot(entry.Payload)
		if err == nil {
			status = s.haVitalsFromSnapshot(snapshot, entry.FetchedAt, loc, "stale")
			status.Warnings = append(status.Warnings, "Using cached Home Assistant states while refresh is paused after recent errors.")
			return status
		}
		status.StateLabel = "backoff"
		status.Warnings = append(status.Warnings, "Home Assistant refresh is paused after recent errors.")
		return status
	}
	if s.retryingKey(cacheKeyHAStates) {
		status.StateLabel = "retrying"
		status.Warnings = append(status.Warnings, "Home Assistant refresh is already retrying.")
		return status
	}

	snapshot, payload, err := s.fetchHASnapshot(ctx, loc, now)
	if err == nil {
		if err := s.apiCache.put(ctx, cacheKeyHAStates, payload, now, s.cfg.HACacheEvery); err != nil {
			s.logger.Warn("store home assistant cache", "error", err)
		}
		return s.haVitalsFromSnapshot(snapshot, now, loc, "live")
	}

	s.logger.Warn("refresh home assistant states", "error", err)
	s.startBackgroundRetry(cacheKeyHAStates, s.cfg.HACacheEvery, func(retryCtx context.Context) ([]byte, error) {
		_, payload, err := s.fetchHASnapshot(retryCtx, loc, time.Now())
		return payload, err
	})
	if ok {
		snapshot, decodeErr := decodeHASnapshot(entry.Payload)
		if decodeErr == nil {
			status = s.haVitalsFromSnapshot(snapshot, entry.FetchedAt, loc, "stale")
			status.Warnings = append(status.Warnings, "Could not refresh Home Assistant; using cached states.")
			return status
		}
	}
	status.StateLabel = "unavailable"
	status.Warnings = append(status.Warnings, "Could not fetch Home Assistant states.")
	return status
}

func (s *server) fetchHASnapshot(parent context.Context, loc *time.Location, now time.Time) (haSnapshot, []byte, error) {
	ctx, cancel := context.WithTimeout(parent, s.cfg.HTTPTimeout)
	defer cancel()

	statesPayload, err := s.fetchHAJSON(ctx, strings.TrimRight(s.cfg.HABaseURL, "/")+"/api/states")
	if err != nil {
		return haSnapshot{}, nil, err
	}
	states, err := decodeHAStates(statesPayload)
	if err != nil {
		return haSnapshot{}, nil, err
	}

	snapshot := haSnapshot{
		States:      states,
		DailyDeltas: make(map[string]string),
	}
	for _, metric := range s.cfg.HAMetrics {
		if strings.EqualFold(strings.TrimSpace(metric.Mode), "daily_delta") {
			value, err := s.fetchHADailyDelta(ctx, metric.EntityID, loc, now)
			if err != nil {
				snapshot.Warnings = append(snapshot.Warnings, "Could not calculate daily Home Assistant value: "+metric.EntityID)
				s.logger.Warn("fetch home assistant daily delta", "entity_id", metric.EntityID, "error", err)
				continue
			}
			snapshot.DailyDeltas[metric.EntityID] = value
		}
	}

	payload, err := json.Marshal(snapshot)
	return snapshot, payload, err
}

func (s *server) fetchHAJSON(ctx context.Context, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.cfg.HAAccessToken)
	req.Header.Set("User-Agent", "emf-dashboard/0.1")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, errors.New(resp.Status)
	}
	return io.ReadAll(resp.Body)
}

func (s *server) fetchHADailyDelta(ctx context.Context, entityID string, loc *time.Location, now time.Time) (string, error) {
	localNow := now.In(loc)
	start := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, loc)

	endpoint, err := url.Parse(strings.TrimRight(s.cfg.HABaseURL, "/") + "/api/history/period/" + url.PathEscape(start.Format(time.RFC3339)))
	if err != nil {
		return "", err
	}
	query := endpoint.Query()
	query.Set("filter_entity_id", entityID)
	query.Set("end_time", localNow.Format(time.RFC3339))
	query.Set("minimal_response", "")
	endpoint.RawQuery = query.Encode()

	payload, err := s.fetchHAJSON(ctx, endpoint.String())
	if err != nil {
		return "", err
	}
	var history [][]haState
	if err := json.Unmarshal(payload, &history); err != nil {
		return "", err
	}
	return computeHADailyDelta(history)
}

func computeHADailyDelta(history [][]haState) (string, error) {
	var first float64
	var latest float64
	found := false

	for _, series := range history {
		for _, point := range series {
			value, ok := parseHAFloat(point.State)
			if !ok {
				continue
			}
			if !found {
				first = value
				found = true
			}
			latest = value
		}
	}
	if !found {
		return "", errors.New("no numeric history states")
	}

	delta := latest - first
	if delta < 0 {
		delta = 0
	}
	return formatHADelta(delta), nil
}

func parseHAFloat(value string) (float64, bool) {
	value = strings.TrimSpace(value)
	if value == "" || value == "unknown" || value == "unavailable" {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

func formatHADelta(delta float64) string {
	rounded := math.Round(delta)
	if math.Abs(delta-rounded) < 0.000001 {
		return strconv.FormatInt(int64(rounded), 10)
	}
	formatted := strconv.FormatFloat(delta, 'f', 2, 64)
	formatted = strings.TrimRight(formatted, "0")
	return strings.TrimRight(formatted, ".")
}

func decodeHAStates(payload []byte) ([]haState, error) {
	var states []haState
	if err := json.Unmarshal(payload, &states); err != nil {
		return nil, err
	}
	return states, nil
}

func decodeHASnapshot(payload []byte) (haSnapshot, error) {
	var snapshot haSnapshot
	if err := json.Unmarshal(payload, &snapshot); err == nil && snapshot.States != nil {
		return snapshot, nil
	}

	states, err := decodeHAStates(payload)
	if err != nil {
		return haSnapshot{}, err
	}
	return haSnapshot{States: states}, nil
}

func (s *server) haVitalsFromSnapshot(snapshot haSnapshot, fetchedAt time.Time, loc *time.Location, source string) haVitals {
	status := haVitals{
		Enabled:       true,
		StateLabel:    source,
		CachedAtLabel: fetchedAt.In(loc).Format("15:04"),
	}
	status.Warnings = append(status.Warnings, snapshot.Warnings...)

	if len(s.cfg.HAMetrics) > 0 {
		for _, metric := range s.cfg.HAMetrics {
			vital, ok := haVitalFromMetric(snapshot, metric)
			if ok {
				status.Items = append(status.Items, vital)
			} else {
				status.Warnings = append(status.Warnings, "Home Assistant entity not found: "+metric.EntityID)
			}
		}
	} else {
		if vital, ok := findHAStateVital(snapshot.States, "Steps", []string{"step", "steps"}); ok {
			status.Items = append(status.Items, vital)
		}
		if vital, ok := findHAStateVital(snapshot.States, "Phone Battery", []string{"battery_level"}); ok {
			status.Items = append(status.Items, vital)
		} else if vital, ok := findHAStateVital(snapshot.States, "Phone Battery", []string{"battery"}); ok {
			status.Items = append(status.Items, vital)
		}
		if vital, ok := findHAStateVital(snapshot.States, "Battery State", []string{"battery_state"}); ok {
			status.Items = append(status.Items, vital)
		} else if vital, ok := findHAStateVital(snapshot.States, "Battery State", []string{"charger", "charging"}); ok {
			status.Items = append(status.Items, vital)
		}
	}

	if len(status.Items) == 0 {
		status.StateLabel = "no metrics"
		status.Warnings = append(status.Warnings, "Home Assistant is reachable, but no configured phone vitals were found.")
	}
	return status
}

func haVitalFromMetric(snapshot haSnapshot, metric haMetricConfig) (haVital, bool) {
	if strings.EqualFold(strings.TrimSpace(metric.Mode), "daily_delta") {
		value, ok := snapshot.DailyDeltas[metric.EntityID]
		if ok && value != "" {
			vital := haVital{
				Label: metric.Label,
				Value: formatStateValue(value, metric.Unit),
				Meta:  metric.EntityID,
			}
			addHAProgress(&vital, value, metric)
			return vital, true
		}
		return haVital{}, false
	}

	for _, state := range snapshot.States {
		if state.EntityID != metric.EntityID {
			continue
		}
		if state.State == "" || state.State == "unknown" || state.State == "unavailable" {
			return haVital{}, false
		}
		unit := metric.Unit
		if unit == "" {
			if value, ok := state.Attributes["unit_of_measurement"].(string); ok {
				unit = value
			}
		}
		vital := haVital{
			Label: metric.Label,
			Value: formatStateValue(state.State, unit),
			Meta:  state.EntityID,
		}
		addHAProgress(&vital, state.State, metric)
		return vital, true
	}
	return haVital{}, false
}

func addHAProgress(vital *haVital, value string, metric haMetricConfig) {
	if !strings.EqualFold(strings.TrimSpace(metric.Display), "progress") {
		return
	}
	percent, ok := haProgressPercent(value)
	if !ok {
		return
	}
	vital.HasProgress = true
	vital.ProgressPercent = percent
}

func haProgressPercent(value string) (int, bool) {
	value = strings.TrimSpace(strings.TrimSuffix(value, "%"))
	if value == "" || value == "unknown" || value == "unavailable" {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, false
	}
	if parsed < 0 {
		parsed = 0
	}
	if parsed > 100 {
		parsed = 100
	}
	return int(math.Round(parsed)), true
}

func findHAStateVital(states []haState, label string, needles []string) (haVital, bool) {
	for _, state := range states {
		haystack := strings.ToLower(state.EntityID + " " + haFriendlyName(state))
		if !strings.Contains(haystack, "pixel") {
			continue
		}
		matched := false
		for _, needle := range needles {
			if strings.Contains(haystack, needle) {
				matched = true
				break
			}
		}
		if !matched || state.State == "" || state.State == "unknown" || state.State == "unavailable" {
			continue
		}

		value := state.State
		if unit, ok := state.Attributes["unit_of_measurement"].(string); ok {
			value = formatStateValue(value, unit)
		}
		return haVital{
			Label: label,
			Value: value,
			Meta:  state.EntityID,
		}, true
	}
	return haVital{}, false
}

func formatStateValue(value string, unit string) string {
	unit = strings.TrimSpace(unit)
	if unit == "" || strings.Contains(value, unit) {
		return value
	}
	switch unit {
	case "%", "°C", "°F":
		return value + unit
	default:
		return value + " " + unit
	}
}

func haFriendlyName(state haState) string {
	if state.Attributes == nil {
		return ""
	}
	if value, ok := state.Attributes["friendly_name"].(string); ok {
		return value
	}
	return ""
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

func fetchRawJSON(ctx context.Context, client *http.Client, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "emf-dashboard/0.1")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, errors.New(resp.Status)
	}
	return io.ReadAll(resp.Body)
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
