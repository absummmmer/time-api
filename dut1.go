package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultDUT1SourceURL = "https://datacenter.iers.org/data/latestVersion/bulletinA.txt"
	defaultDUT1Refresh   = 6 * time.Hour
	defaultDUT1Timeout   = 20 * time.Second
	defaultNTSCBaseURL   = "http://www.ut1.ntsc.ac.cn"
)

// DUT1Point is one daily UT1-UTC value published by an EOP source.
type DUT1Point struct {
	Date  string  `json:"date"`
	MJD   int     `json:"mjd,omitempty"`
	Value float64 `json:"value"` // UT1-UTC in seconds
	DUT1  float64 `json:"dut1"`  // UT1-UTC rounded to the 0.1 s DUT1 step used by time signals
	Type  string  `json:"type"`  // "observed" or "predicted"
}

// DUT1SourceSnapshot is the alternate source's view of the same UTC date.
// It lets the UI show both sources and switch without a second round-trip.
type DUT1SourceSnapshot struct {
	Source    string     `json:"source"`
	SourceKey string     `json:"source_key,omitempty"`
	UpdatedAt string     `json:"updated_at,omitempty"`
	Stale     bool       `json:"stale"`
	Available bool       `json:"available"`
	Previous  *DUT1Point `json:"previous,omitempty"`
	Current   *DUT1Point `json:"current,omitempty"`
	Next      *DUT1Point `json:"next,omitempty"`
	Error     string     `json:"error,omitempty"`
}

// DUT1Info is the API representation of the DUT1 value for one UTC date and
// its two neighbouring dates. The neighbouring values let a client roll over
// at UTC midnight without performing an extra request.
type DUT1Info struct {
	Date       string              `json:"date"`
	DateBasis  string              `json:"date_basis"`
	Unit       string              `json:"unit"`
	Source     string              `json:"source"`
	SourceKey  string              `json:"source_key,omitempty"`
	SourceMode string              `json:"source_mode,omitempty"`
	SourceURL  string              `json:"source_url,omitempty"`
	UpdatedAt  string              `json:"updated_at,omitempty"`
	Stale      bool                `json:"stale"`
	Available  bool                `json:"available"`
	Previous   *DUT1Point          `json:"previous"`
	Current    *DUT1Point          `json:"current"`
	Next       *DUT1Point          `json:"next"`
	Alternate  *DUT1SourceSnapshot `json:"alternate,omitempty"`
	Error      string              `json:"error,omitempty"`
}

// dut1Service periodically downloads and caches an Earth-orientation data
// file (IERS Bulletin A by default) and serves DUT1 values from memory.
type dut1Service struct {
	mu        sync.RWMutex
	points    map[string]DUT1Point // key: YYYY-MM-DD (UTC)
	source    string
	sourceKey string
	sourceURL string
	updatedAt time.Time
	lastErr   error

	mode        string
	ntscBaseURL string
	urls        []string
	refresh     time.Duration
	staleAfter  time.Duration
	timeout     time.Duration
	client      *http.Client
	logger      *JSONLogger
}

func newDUT1Service(logger *JSONLogger) *dut1Service {
	urls := []string{defaultDUT1SourceURL}
	if raw := strings.TrimSpace(os.Getenv("DUT1_SOURCE_URL")); raw != "" {
		custom := make([]string, 0, 2)
		for _, u := range strings.Split(raw, ",") {
			if u = strings.TrimSpace(u); u != "" {
				custom = append(custom, u)
			}
		}
		if len(custom) > 0 {
			urls = custom
		}
	}

	refresh := envDuration("DUT1_REFRESH_INTERVAL", defaultDUT1Refresh)
	if refresh < 5*time.Minute {
		refresh = 5 * time.Minute
	}
	timeout := envDuration("DUT1_HTTP_TIMEOUT", defaultDUT1Timeout)
	if timeout < 5*time.Second {
		timeout = 5 * time.Second
	}
	staleAfter := envDuration("DUT1_STALE_AFTER", 2*refresh)
	if staleAfter < refresh {
		staleAfter = 2 * refresh
	}
	ntscBaseURL := strings.TrimSpace(os.Getenv("DUT1_NTSC_BASE_URL"))
	if ntscBaseURL == "" {
		ntscBaseURL = defaultNTSCBaseURL
	}

	return &dut1Service{
		mode:        "url",
		sourceKey:   "iers",
		ntscBaseURL: strings.TrimRight(ntscBaseURL, "/"),
		urls:        urls,
		refresh:     refresh,
		staleAfter:  staleAfter,
		timeout:     timeout,
		client: &http.Client{
			Timeout: timeout,
		},
		logger: logger,
	}
}

func newNTSCDUT1Service(logger *JSONLogger) *dut1Service {
	service := newDUT1Service(logger)
	service.mode = "ntsc"
	service.sourceKey = "ntsc"
	service.urls = nil
	return service
}

func envDuration(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

// Start performs an initial, best-effort refresh and then keeps the cache up
// to date in the background. A failed initial refresh does not stop the
// server; the request handlers simply report that DUT1 is unavailable.
func (s *dut1Service) Start(ctx context.Context) {
	s.logger.Println("Initializing DUT1 service...")
	initCtx, cancel := context.WithTimeout(ctx, s.timeout)
	err := s.refreshOnce(initCtx)
	cancel()

	delay := s.refresh
	if err != nil {
		s.logger.Error("", "Initial DUT1 refresh failed", map[string]interface{}{"error": err.Error()})
		delay = 2 * time.Minute
	}
	go s.run(ctx, delay)
}

func (s *dut1Service) run(ctx context.Context, delay time.Duration) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		refreshCtx, cancel := context.WithTimeout(ctx, s.timeout)
		err := s.refreshOnce(refreshCtx)
		cancel()
		if err != nil {
			s.logger.Error("", "DUT1 refresh failed", map[string]interface{}{"error": err.Error()})
			delay = 5 * time.Minute
			continue
		}
		delay = s.refresh
	}
}

func (s *dut1Service) refreshOnce(ctx context.Context) error {
	if s.mode == "ntsc" {
		return s.refreshNTSC(ctx)
	}

	var failures []string
	for _, url := range s.urls {
		if err := ctx.Err(); err != nil {
			return err
		}
		body, err := s.fetchSource(ctx, url)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", url, err))
			continue
		}
		points, err := parseDUT1Text(string(body))
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", url, err))
			continue
		}

		s.storePoints(points, dut1SourceName(url), url)
		s.logger.Info("", "DUT1 data loaded", map[string]interface{}{
			"source": url,
			"days":   len(points),
		})
		return nil
	}

	err := fmt.Errorf("all DUT1 sources failed: %s", strings.Join(failures, "; "))
	s.mu.Lock()
	s.lastErr = err
	s.mu.Unlock()
	return err
}

func (s *dut1Service) storePoints(points map[string]DUT1Point, source, sourceURL string) {
	s.mu.Lock()
	s.points = points
	s.source = source
	s.sourceURL = sourceURL
	s.updatedAt = time.Now().UTC()
	s.lastErr = nil
	s.mu.Unlock()
}

func (s *dut1Service) refreshNTSC(ctx context.Context) error {
	body, downloadURL, err := s.fetchNTSCFile(ctx)
	if err != nil {
		s.mu.Lock()
		s.lastErr = err
		s.mu.Unlock()
		return err
	}
	points, err := parseDUT1Text(string(body))
	if err != nil {
		wrapped := fmt.Errorf("parse NTSC EOP file: %w", err)
		s.mu.Lock()
		s.lastErr = wrapped
		s.mu.Unlock()
		return wrapped
	}
	s.storePoints(points, "NTSC EOP", downloadURL)
	s.logger.Info("", "NTSC DUT1 data loaded", map[string]interface{}{
		"source": downloadURL,
		"days":   len(points),
	})
	return nil
}

type ntscDataListEntry struct {
	ID         int    `json:"id"`
	FileName   string `json:"fileName"`
	Path       string `json:"path"`
	CreateTime string `json:"createTime"`
}

type ntscDataListResponse struct {
	Code int                 `json:"code"`
	Data []ntscDataListEntry `json:"data"`
}

func (s *dut1Service) fetchNTSCFile(ctx context.Context) ([]byte, string, error) {
	listURL := s.ntscBaseURL + "/ftpFile/findDataList"
	payload := `{"page":1,"pageSize":10,"query":{"type":1}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, listURL, strings.NewReader(payload))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "time-api/1.1 (DUT1 provider)")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("NTSC data list request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("NTSC data list HTTP %s", resp.Status)
	}
	var list ntscDataListResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&list); err != nil {
		return nil, "", fmt.Errorf("decode NTSC data list: %w", err)
	}
	if list.Code != 0 {
		return nil, "", fmt.Errorf("NTSC data list returned code %d", list.Code)
	}

	var latest *ntscDataListEntry
	for index := range list.Data {
		entry := &list.Data[index]
		name := strings.TrimSpace(entry.FileName)
		if !strings.HasPrefix(name, "NTSC_EOP_") || len(name) != len("NTSC_EOP_")+8 {
			continue
		}
		if !isDigits(name[len("NTSC_EOP_"):]) {
			continue
		}
		if entry.ID <= 0 {
			continue
		}
		if latest == nil || entry.ID > latest.ID ||
			(entry.ID == latest.ID && entry.CreateTime > latest.CreateTime) {
			latest = entry
		}
	}
	if latest == nil {
		return nil, "", fmt.Errorf("no NTSC_EOP_yyyyMMdd file found in data list")
	}

	downloadURL := fmt.Sprintf("%s/ftpFile/download?fileId=%d", s.ntscBaseURL, latest.ID)
	body, err := s.fetchSource(ctx, downloadURL)
	if err != nil {
		return nil, "", fmt.Errorf("download %s: %w", latest.FileName, err)
	}
	return body, downloadURL, nil
}

func (s *dut1Service) fetchSource(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "time-api/1.1 (DUT1 provider)")
	req.Header.Set("Accept", "text/plain, application/octet-stream, */*")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected HTTP status %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("empty response body")
	}
	return body, nil
}

func dut1SourceName(sourceURL string) string {
	lower := strings.ToLower(sourceURL)
	switch {
	case strings.Contains(lower, "ntsc"):
		return "NTSC EOP"
	case strings.Contains(lower, "bulletin"):
		return "IERS Bulletin A"
	default:
		return "DUT1 source"
	}
}

// Info returns the DUT1 values for at.UTC() and for the UTC dates immediately
// before and after it.
func (s *dut1Service) Info(at time.Time) DUT1Info {
	utc := at.UTC()
	date := utc.Format("2006-01-02")

	s.mu.RLock()
	points := s.points
	source := s.source
	sourceKey := s.sourceKey
	sourceURL := s.sourceURL
	updatedAt := s.updatedAt
	lastErr := s.lastErr
	s.mu.RUnlock()

	info := DUT1Info{
		Date:      date,
		DateBasis: "UTC",
		Unit:      "s",
		Source:    source,
		SourceKey: sourceKey,
		SourceURL: sourceURL,
		Stale:     true,
	}
	if info.Source == "" {
		info.Source = "IERS Bulletin A"
	}
	if !updatedAt.IsZero() {
		info.UpdatedAt = updatedAt.Format(time.RFC3339)
		info.Stale = time.Since(updatedAt) > s.staleAfter
	}

	if len(points) == 0 {
		info.Error = "DUT1 data not loaded yet"
		if lastErr != nil {
			info.Error = lastErr.Error()
		}
		return info
	}

	info.Previous = pointForDate(points, utc.AddDate(0, 0, -1))
	info.Current = pointForDate(points, utc)
	info.Next = pointForDate(points, utc.AddDate(0, 0, 1))
	info.Available = info.Current != nil
	if !info.Available {
		info.Error = fmt.Sprintf("no DUT1 data available for %s", date)
	}
	return info
}

func pointForDate(points map[string]DUT1Point, at time.Time) *DUT1Point {
	p, ok := points[at.Format("2006-01-02")]
	if !ok {
		return nil
	}
	out := p
	return &out
}

func alternateDUT1Snapshot(activeKey string, at time.Time) *DUT1SourceSnapshot {
	var other *dut1Service
	switch activeKey {
	case "ntsc":
		other = dut1
	case "iers":
		other = dut1NTSC
	default:
		return nil
	}
	if other == nil {
		return nil
	}
	info := other.Info(at)
	return &DUT1SourceSnapshot{
		Source:    info.Source,
		SourceKey: info.SourceKey,
		UpdatedAt: info.UpdatedAt,
		Stale:     info.Stale,
		Available: info.Available,
		Previous:  info.Previous,
		Current:   info.Current,
		Next:      info.Next,
		Error:     info.Error,
	}
}

func normalizeDUT1SourceMode(raw string) (string, bool) {
	mode := strings.ToLower(strings.TrimSpace(raw))
	if mode == "" {
		mode = strings.ToLower(strings.TrimSpace(os.Getenv("DUT1_DEFAULT_SOURCE")))
		if mode == "" {
			mode = "auto"
		}
	}
	switch mode {
	case "auto", "ntsc", "iers":
		return mode, true
	default:
		return "", false
	}
}

func dut1InfoFor(at time.Time, mode string) DUT1Info {
	normalized, ok := normalizeDUT1SourceMode(mode)
	if !ok {
		normalized = "auto"
	}
	var service *dut1Service
	switch normalized {
	case "ntsc":
		service = dut1NTSC
	case "iers":
		service = dut1
	default:
		if dut1NTSC != nil {
			candidate := dut1NTSC.Info(at)
			if candidate.Available && !candidate.Stale {
				service = dut1NTSC
			}
		}
		if service == nil {
			service = dut1
		}
	}
	if service == nil {
		return DUT1Info{
			Date:       at.UTC().Format("2006-01-02"),
			DateBasis:  "UTC",
			Unit:       "s",
			SourceMode: normalized,
			Stale:      true,
			Error:      "DUT1 source is not available",
		}
	}
	info := service.Info(at)
	info.SourceMode = normalized
	info.Alternate = alternateDUT1Snapshot(info.SourceKey, at)
	return info
}

// parseDUT1Text understands both IERS Bulletin A (observed + prediction
// tables) and the 16-column NTSC EOP text files. It returns a map keyed by
// UTC date (YYYY-MM-DD).
func parseDUT1Text(text string) (map[string]DUT1Point, error) {
	points := make(map[string]DUT1Point)
	section := ""
	var lastRealDate time.Time

	scanner := bufio.NewScanner(strings.NewReader(text))
	scanner.Buffer(make([]byte, 64*1024), 1<<20)

	for scanner.Scan() {
		line := scanner.Text()
		upper := strings.ToUpper(strings.TrimSpace(line))

		if strings.Contains(upper, "LAST DATE WITH REAL DATA:") {
			if separator := strings.LastIndex(line, ":"); separator >= 0 {
				raw := strings.TrimSpace(line[separator+1:])
				if parsed, err := time.Parse("2006-01-02", raw); err == nil {
					lastRealDate = parsed
				}
			}
			continue
		}

		switch {
		case strings.Contains(upper, "COMBINED EARTH ORIENTATION PARAMETERS"):
			section = "observed"
			continue
		case strings.HasPrefix(upper, "PREDICTIONS"):
			section = "predicted"
			continue
		case strings.Contains(upper, "CELESTIAL POLE OFFSET"):
			section = ""
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 7 {
			continue
		}
		yearText := fields[0]
		if !isDigits(yearText) || (len(yearText) != 2 && len(yearText) != 4) {
			continue
		}

		year, _ := strconv.Atoi(yearText)
		if len(yearText) == 2 {
			if year < 70 {
				year += 2000
			} else {
				year += 1900
			}
		}
		month, err1 := strconv.Atoi(fields[1])
		day, err2 := strconv.Atoi(fields[2])
		if err1 != nil || err2 != nil || month < 1 || month > 12 || day < 1 || day > 31 {
			continue
		}
		date := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
		if date.Year() != year || int(date.Month()) != month || date.Day() != day {
			continue
		}

		valueIndex := -1
		pointType := ""
		switch section {
		case "observed":
			// Bulletin A observed rows use a two-digit year and put UT1-UTC
			// in column 8. NTSC files use a four-digit year and 16 columns.
			if len(yearText) == 4 && len(fields) >= 16 {
				valueIndex = 6
			} else if len(fields) >= 10 {
				valueIndex = 8
			} else if len(fields) >= 7 {
				valueIndex = 6
			}
			pointType = "observed"
		case "predicted":
			if len(fields) >= 7 {
				valueIndex = 6
			}
			pointType = "predicted"
		default:
			switch {
			case len(yearText) == 2 && len(fields) >= 10:
				valueIndex = 8
				pointType = "observed"
			case len(yearText) == 4 && len(fields) >= 16:
				valueIndex = 6
				pointType = "observed"
				if !lastRealDate.IsZero() && date.After(lastRealDate) {
					pointType = "predicted"
				}
			case len(yearText) == 4 && len(fields) >= 7:
				valueIndex = 6
				pointType = "predicted"
			}
		}
		if valueIndex < 0 || len(fields) <= valueIndex {
			continue
		}

		value, err := strconv.ParseFloat(fields[valueIndex], 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) > 1.5 {
			continue
		}

		mjd := 0
		if len(fields) > 3 && isDigits(fields[3]) {
			mjd, _ = strconv.Atoi(fields[3])
		}

		point := DUT1Point{
			Date:  date.Format("2006-01-02"),
			MJD:   mjd,
			Value: value,
			DUT1:  roundDUT1(value),
			Type:  pointType,
		}

		// Keep an observed value if the same date appears again in the
		// prediction table.
		if previous, ok := points[point.Date]; ok && previous.Type == "observed" && point.Type == "predicted" {
			continue
		}
		points[point.Date] = point
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(points) == 0 {
		return nil, fmt.Errorf("no DUT1 values found (unsupported or empty data format)")
	}
	return points, nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func roundDUT1(value float64) float64 {
	rounded := math.Round(value*10) / 10
	if rounded == 0 {
		return 0
	}
	return rounded
}

// dut1V1Handler serves the DUT1 endpoint. The optional "date" query parameter
// is interpreted as a UTC calendar date in YYYY-MM-DD format.
func dut1V1Handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")

	requestID := w.Header().Get("X-Request-ID")
	at := time.Now()
	if raw := strings.TrimSpace(r.URL.Query().Get("date")); raw != "" {
		parsed, err := time.Parse("2006-01-02", raw)
		if err != nil {
			Logger.Printf("[%s] Error: invalid DUT1 date '%s'", requestID, raw)
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(ErrorResponse{Error: "Invalid date, expected YYYY-MM-DD"})
			return
		}
		at = parsed
	}
	mode, validMode := normalizeDUT1SourceMode(r.URL.Query().Get("source"))
	if !validMode {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(ErrorResponse{Error: "Invalid source, expected auto, ntsc, or iers"})
		return
	}

	info := dut1InfoFor(at, mode)
	Logger.Printf("[%s] Processing DUT1 request for %s (source=%s, available=%v, stale=%v)", requestID, info.Date, mode, info.Available, info.Stale)
	if err := json.NewEncoder(w).Encode(info); err != nil {
		Logger.Printf("[%s] Error encoding DUT1 response: %v", requestID, err)
	}
}
