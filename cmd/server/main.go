package main

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

type config struct {
	Addr              string
	OwnerName         string
	PublicBaseURL     string
	PublicHostname    string
	DashboardHostname string
	FavouritesURL     string
	BarAPIBaseURL     string
	BarEnabled        bool
	BarPollEvery      time.Duration
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

type barCache struct {
	mu       sync.Mutex
	snapshot barSnapshot
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
	barCache  barCache
}

func main() {
	cfg := loadConfig()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	tmpl := template.Must(template.ParseFiles(
		"web/templates/dashboard.html",
		"web/templates/contact.html",
	))
	srv := &server{
		cfg:       cfg,
		logger:    logger,
		client:    &http.Client{Timeout: cfg.HTTPTimeout},
		templates: tmpl,
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
		EventTZ:           "Europe/London",
		HTTPTimeout:       12 * time.Second,
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
		Mode:             modeLabel(now),
		Signal:           signalLabel(s.cfg.FavouritesURL, warnings),
		SourceLabel:      source,
		LastUpdatedLabel: now.Format("Mon 15:04"),
		Schedule:         firstN(schedule, 6),
		Unscheduled:      firstNUnscheduled(unscheduled, 5),
		FavouriteCount:   len(faves),
		Bar:              bar,
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

func (s *server) loadFavourites(ctx context.Context) ([]favourite, string, []string) {
	if strings.TrimSpace(s.cfg.FavouritesURL) == "" {
		return sampleFavourites(), "sample data", []string{"Set external_apis.emf.favourites_url in config/config.yaml to render your real favourites."}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.FavouritesURL, nil)
	if err != nil {
		return sampleFavourites(), "sample data", []string{"Favourites URL is invalid; showing sample data."}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "emf-dashboard/0.1")

	resp, err := s.client.Do(req)
	if err != nil {
		s.logger.Warn("fetch favourites", "error", err)
		return sampleFavourites(), "sample data", []string{"Could not fetch favourites; showing sample data."}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		s.logger.Warn("fetch favourites status", "status", resp.StatusCode)
		return sampleFavourites(), "sample data", []string{"Favourites endpoint returned an error; showing sample data."}
	}

	var faves []favourite
	if err := json.NewDecoder(resp.Body).Decode(&faves); err != nil {
		s.logger.Warn("decode favourites", "error", err)
		return sampleFavourites(), "sample data", []string{"Could not decode favourites JSON; showing sample data."}
	}

	return faves, "live favourites", nil
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
	s.barCache.mu.Lock()
	defer s.barCache.mu.Unlock()

	if !s.barCache.snapshot.FetchedAt.IsZero() && now.Sub(s.barCache.snapshot.FetchedAt) < s.cfg.BarPollEvery {
		return s.barCache.snapshot, nil
	}

	snapshot, warnings := s.fetchBarSnapshot(ctx, now)
	if snapshot.FetchedAt.IsZero() {
		if !s.barCache.snapshot.FetchedAt.IsZero() {
			warnings = append(warnings, "Using stale bar data.")
			return s.barCache.snapshot, warnings
		}
		return barSnapshot{FetchedAt: now}, warnings
	}

	s.barCache.snapshot = snapshot
	return snapshot, warnings
}

func (s *server) fetchBarSnapshot(ctx context.Context, now time.Time) (barSnapshot, []string) {
	snapshot := barSnapshot{FetchedAt: now}
	var warnings []string

	sessions, err := fetchJSON[barSessionsResponse](ctx, s.client, s.cfg.BarAPIBaseURL, "/api/sessions.json")
	if err != nil {
		warnings = append(warnings, "Bar sessions unavailable.")
		s.logger.Warn("fetch bar sessions", "error", err)
	} else {
		snapshot.Sessions = sessions.Sessions
	}

	progress, err := fetchJSON[barProgressResponse](ctx, s.client, s.cfg.BarAPIBaseURL, "/api/progress.json")
	if err != nil {
		warnings = append(warnings, "Bar progress unavailable.")
		s.logger.Warn("fetch bar progress", "error", err)
	} else {
		snapshot.Progress = progress
	}

	onTap, err := fetchJSON[barOnTapResponse](ctx, s.client, s.cfg.BarAPIBaseURL, "/api/on-tap.json")
	if err != nil {
		warnings = append(warnings, "Main bar on-tap list unavailable.")
		s.logger.Warn("fetch on tap", "error", err)
	} else {
		snapshot.MainTap = onTap
	}

	cybarTap, err := fetchJSON[barOnTapResponse](ctx, s.client, s.cfg.BarAPIBaseURL, "/api/cybar-on-tap.json")
	if err != nil {
		warnings = append(warnings, "Null Sector on-tap list unavailable.")
		s.logger.Warn("fetch cybar on tap", "error", err)
	} else {
		snapshot.CybarTap = cybarTap
	}

	clubMate, err := fetchJSON[barDepartmentResponse](ctx, s.client, s.cfg.BarAPIBaseURL, "/api/department/75.json")
	if err != nil {
		warnings = append(warnings, "Club Mate stock unavailable.")
		s.logger.Warn("fetch club mate", "error", err)
	} else {
		snapshot.ClubMate = clubMate.StockTypes
	}

	return snapshot, warnings
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
				StartLabel:  start.Format("15:04"),
				EndLabel:    end.Format("15:04"),
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
	if d < time.Hour {
		return "T-" + d.Round(time.Minute).String()
	}
	if d < 48*time.Hour {
		hours := int(d.Hours())
		mins := int(d.Minutes()) % 60
		if mins == 0 {
			return "T-" + (time.Duration(hours) * time.Hour).String()
		}
		return "T-" + (time.Duration(hours) * time.Hour).String() + " " + (time.Duration(mins) * time.Minute).String()
	}
	return start.Format("Mon 2 Jan")
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
