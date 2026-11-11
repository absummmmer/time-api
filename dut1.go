package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultDUT1SourceURL     = "https://datacenter.iers.org/data/latestVersion/bulletinA.txt"
	defaultDUT1Refresh       = 6 * time.Hour
	defaultDUT1Timeout       = 20 * time.Second
	defaultNTSCRefresh       = 24 * time.Hour
	defaultNTSCRetry         = time.Hour
	defaultIERSMirrorRefresh = 24 * time.Hour
	defaultIERSMirrorRetry   = time.Hour
	defaultNTSCBaseURL       = "http://www.ut1.ntsc.ac.cn"
)

// dut1CacheSnapshot is the on-disk form of one provider's last-good data. It
// lets the API keep serving DUT1 after a restart while both upstream networks
// are unreachable, without downloading the source again on every boot.
type dut1CacheSnapshot struct {
	Source         string               `json:"source"`
	SourceKey      string               `json:"source_key"`
	SourceURL      string               `json:"source_url"`
	UpdatedAt      time.Time            `json:"updated_at"`
	Points         map[string]DUT1Point `json:"points"`
	NTSCLatestID   int                  `json:"ntsc_latest_id,omitempty"`
	NTSCLatestName string               `json:"ntsc_latest_name,omitempty"`
	IERSMirrorURL  string               `json:"iers_mirror_url,omitempty"`
	IERSMirrorName string               `json:"iers_mirror_name,omitempty"`
	IERSMirrorAt   time.Time            `json:"iers_mirror_at,omitempty"`
}

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
	retry       time.Duration
	staleAfter  time.Duration
	timeout     time.Duration
	client      *http.Client
	logger      *JSONLogger
	cacheDir    string

	startOnce      sync.Once
	ntscLatestID   int
	ntscLatestName string

	iersMirrorEnabled   bool
	iersMirrorRefresh   time.Duration
	iersMirrorRetry     time.Duration
	iersMirrorURL       string
	iersMirrorName      string
	iersMirrorAt        time.Time
	iersMirrorAttemptAt time.Time
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
	mirrorRefresh := envDuration("DUT1_IERS_MIRROR_REFRESH_INTERVAL", defaultIERSMirrorRefresh)
	if mirrorRefresh < time.Hour {
		mirrorRefresh = time.Hour
	}
	mirrorRetry := envDuration("DUT1_IERS_MIRROR_RETRY_INTERVAL", defaultIERSMirrorRetry)
	if mirrorRetry < 5*time.Minute {
		mirrorRetry = 5 * time.Minute
	}

	return &dut1Service{
		mode:        "url",
		sourceKey:   "iers",
		ntscBaseURL: strings.TrimRight(ntscBaseURL, "/"),
		urls:        urls,
		refresh:     refresh,
		retry:       5 * time.Minute,
		staleAfter:  staleAfter,
		timeout:     timeout,
		client: &http.Client{
			Timeout: timeout,
		},
		logger:            logger,
		cacheDir:          strings.TrimSpace(os.Getenv("DUT1_CACHE_DIR")),
		iersMirrorEnabled: envBool("DUT1_IERS_MIRROR_ENABLED", true),
		iersMirrorRefresh: mirrorRefresh,
		iersMirrorRetry:   mirrorRetry,
	}
}

func newNTSCDUT1Service(logger *JSONLogger) *dut1Service {
	service := newDUT1Service(logger)
	service.mode = "ntsc"
	service.sourceKey = "ntsc"
	service.urls = nil
	service.refresh = envDuration("DUT1_NTSC_REFRESH_INTERVAL", defaultNTSCRefresh)
	if service.refresh < time.Hour {
		service.refresh = time.Hour
	}
	service.retry = envDuration("DUT1_NTSC_RETRY_INTERVAL", defaultNTSCRetry)
	if service.retry < 5*time.Minute {
		service.retry = 5 * time.Minute
	}
	service.staleAfter = envDuration("DUT1_NTSC_STALE_AFTER", 2*service.refresh)
	if service.staleAfter < service.refresh {
		service.staleAfter = 2 * service.refresh
	}
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
// server; the request handlers report unavailable/stale data and the refresh
// loop retries later. If a usable disk cache exists, the initial network fetch
// is deferred until the cache expires.
func (s *dut1Service) Start(ctx context.Context) {
	s.startOnce.Do(func() { s.start(ctx) })
}

// EnsureStarted starts a service asynchronously exactly once. It is used for
// the NTSC fallback so that, while the primary IERS source is healthy, NTSC is
// loaded from disk but never contacted.
func (s *dut1Service) EnsureStarted() {
	s.startOnce.Do(func() {
		go s.start(context.Background())
	})
}

func (s *dut1Service) start(ctx context.Context) {
	s.LoadCache()
	s.logger.Println("Initializing DUT1 service...")

	delay := s.refresh
	if s.cacheIsFresh(time.Now()) {
		age := s.cacheAge()
		delay = s.refresh - age
		if delay < time.Minute {
			delay = time.Minute
		}
		s.logger.Info("", "DUT1 cache is fresh; deferring upstream refresh", map[string]interface{}{
			"source_key": s.sourceKey,
			"cache_age":  age.String(),
		})
	} else {
		initCtx, cancel := context.WithTimeout(ctx, s.timeout)
		err := s.refreshOnce(initCtx)
		cancel()
		if err != nil {
			s.logger.Error("", "Initial DUT1 refresh failed", map[string]interface{}{"error": err.Error()})
			delay = s.retry
		}
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
			delay = s.retry
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

	// The IERS official host can be unreachable from some networks. NTSC keeps
	// a mirror of the same IERS Bulletin A files; only use it after every
	// configured IERS URL failed, so the primary path stays authoritative.
	if s.iersMirrorEnabled {
		if err := s.refreshIERSMirror(ctx); err == nil {
			return nil
		} else {
			failures = append(failures, "NTSC IERS mirror: "+err.Error())
		}
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
	s.saveCache()
}

func (s *dut1Service) cachePath() string {
	if s.cacheDir == "" {
		return ""
	}
	key := s.sourceKey
	if key == "" {
		key = "dut1"
	}
	return filepath.Join(s.cacheDir, "dut1-"+key+".json")
}

func (s *dut1Service) cacheAge() time.Duration {
	s.mu.RLock()
	updatedAt := s.updatedAt
	s.mu.RUnlock()
	if updatedAt.IsZero() {
		return 0
	}
	return time.Since(updatedAt)
}

// cacheIsFresh accepts a disk/network cache only when it is younger than the
// refresh interval and still covers today's UTC date. That prevents a restart
// from triggering an unnecessary upstream request while still refreshing when
// the cached table has rolled past its last available day.
func (s *dut1Service) cacheIsFresh(now time.Time) bool {
	s.mu.RLock()
	updatedAt := s.updatedAt
	points := s.points
	s.mu.RUnlock()
	if updatedAt.IsZero() || len(points) == 0 || now.Sub(updatedAt) >= s.refresh {
		return false
	}
	_, hasToday := points[now.UTC().Format("2006-01-02")]
	return hasToday
}

// LoadCache restores the last-good snapshot written by saveCache. It is safe
// to call more than once; an already-populated in-memory cache is kept.
func (s *dut1Service) LoadCache() {
	path := s.cachePath()
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			s.logger.Error("", "DUT1 cache read failed", map[string]interface{}{"path": path, "error": err.Error()})
		}
		return
	}
	var snapshot dut1CacheSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		s.logger.Error("", "DUT1 cache decode failed", map[string]interface{}{"path": path, "error": err.Error()})
		return
	}
	if snapshot.SourceKey != "" && s.sourceKey != "" && snapshot.SourceKey != s.sourceKey {
		return
	}
	if len(snapshot.Points) == 0 {
		return
	}
	source := snapshot.Source
	if source == "" {
		if s.sourceKey == "ntsc" {
			source = "NTSC EOP"
		} else {
			source = "IERS Bulletin A"
		}
	}
	s.mu.Lock()
	if len(s.points) == 0 {
		s.points = snapshot.Points
		s.source = source
		s.sourceURL = snapshot.SourceURL
		s.updatedAt = snapshot.UpdatedAt
		s.ntscLatestID = snapshot.NTSCLatestID
		s.ntscLatestName = snapshot.NTSCLatestName
		s.iersMirrorURL = snapshot.IERSMirrorURL
		s.iersMirrorName = snapshot.IERSMirrorName
		s.iersMirrorAt = snapshot.IERSMirrorAt
	}
	s.mu.Unlock()
	s.logger.Info("", "DUT1 cache restored", map[string]interface{}{
		"path":       path,
		"source_key": s.sourceKey,
		"days":       len(snapshot.Points),
		"updated_at": snapshot.UpdatedAt.Format(time.RFC3339),
	})
}

// saveCache writes the last-good snapshot atomically so a Docker restart can
// serve DUT1 immediately even while both upstream providers are unreachable.
func (s *dut1Service) saveCache() {
	path := s.cachePath()
	if path == "" {
		return
	}
	s.mu.RLock()
	snapshot := dut1CacheSnapshot{
		Source:         s.source,
		SourceKey:      s.sourceKey,
		SourceURL:      s.sourceURL,
		UpdatedAt:      s.updatedAt,
		Points:         s.points,
		NTSCLatestID:   s.ntscLatestID,
		NTSCLatestName: s.ntscLatestName,
		IERSMirrorURL:  s.iersMirrorURL,
		IERSMirrorName: s.iersMirrorName,
		IERSMirrorAt:   s.iersMirrorAt,
	}
	s.mu.RUnlock()
	if len(snapshot.Points) == 0 {
		return
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		s.logger.Error("", "DUT1 cache encode failed", map[string]interface{}{"error": err.Error()})
		return
	}
	if err := os.MkdirAll(s.cacheDir, 0o755); err != nil {
		s.logger.Error("", "DUT1 cache directory create failed", map[string]interface{}{"path": s.cacheDir, "error": err.Error()})
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		s.logger.Error("", "DUT1 cache write failed", map[string]interface{}{"path": tmp, "error": err.Error()})
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		s.logger.Error("", "DUT1 cache rename failed", map[string]interface{}{"path": path, "error": err.Error()})
	}
}

func (s *dut1Service) refreshNTSC(ctx context.Context) error {
	latest, err := s.fetchNTSCList(ctx)
	if err != nil {
		s.mu.Lock()
		s.lastErr = err
		s.mu.Unlock()
		return err
	}

	// The NTSC site publishes one large daily EOP file. A metadata-only list
	// request is enough to know whether a new file exists; skip the multi-MB
	// download when the file ID is unchanged. This keeps the fallback source
	// low-traffic even when it is queried regularly.
	downloadURL := fmt.Sprintf("%s/ftpFile/download?fileId=%d", s.ntscBaseURL, latest.ID)
	s.mu.RLock()
	cachedID := s.ntscLatestID
	hasCachedPoints := len(s.points) > 0
	s.mu.RUnlock()
	if latest.ID > 0 && latest.ID == cachedID && hasCachedPoints {
		s.logger.Info("", "NTSC EOP file unchanged; download skipped", map[string]interface{}{
			"file": latest.FileName,
			"id":   latest.ID,
		})
		return nil
	}

	body, err := s.fetchSource(ctx, downloadURL)
	if err != nil {
		wrapped := fmt.Errorf("download %s: %w", latest.FileName, err)
		s.mu.Lock()
		s.lastErr = wrapped
		s.mu.Unlock()
		return wrapped
	}
	points, err := parseDUT1Text(string(body))
	if err != nil {
		wrapped := fmt.Errorf("parse NTSC EOP file: %w", err)
		s.mu.Lock()
		s.lastErr = wrapped
		s.mu.Unlock()
		return wrapped
	}
	s.mu.Lock()
	s.ntscLatestID = latest.ID
	s.ntscLatestName = latest.FileName
	s.mu.Unlock()
	s.storePoints(points, "NTSC EOP", downloadURL)
	s.logger.Info("", "NTSC DUT1 data loaded", map[string]interface{}{
		"source": downloadURL,
		"file":   latest.FileName,
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

func (s *dut1Service) fetchNTSCList(ctx context.Context) (*ntscDataListEntry, error) {
	listURL := s.ntscBaseURL + "/ftpFile/findDataList"
	payload := `{"page":1,"pageSize":10,"query":{"type":1}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, listURL, strings.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "time-api/1.1 (DUT1 provider)")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("NTSC data list request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("NTSC data list HTTP %s", resp.Status)
	}
	var list ntscDataListResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&list); err != nil {
		return nil, fmt.Errorf("decode NTSC data list: %w", err)
	}
	if list.Code != 0 {
		return nil, fmt.Errorf("NTSC data list returned code %d", list.Code)
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
		return nil, fmt.Errorf("no NTSC_EOP_yyyyMMdd file found in data list")
	}
	return latest, nil
}

// refreshIERSMirror obtains the IERS Bulletin A from NTSC's mirror. It runs
// only after every configured IERS URL failed. Listings and the bulletin file
// are cached; the mirror is re-resolved at most once per mirror refresh
// interval, so a long IERS outage does not turn into a polling loop.
func (s *dut1Service) refreshIERSMirror(ctx context.Context) error {
	now := time.Now().UTC()

	s.mu.RLock()
	mirrorAt := s.iersMirrorAt
	attemptAt := s.iersMirrorAttemptAt
	hasPoints := len(s.points) > 0
	s.mu.RUnlock()

	if !mirrorAt.IsZero() && now.Sub(mirrorAt) < s.iersMirrorRefresh {
		return nil
	}
	if !attemptAt.IsZero() && now.Sub(attemptAt) < s.iersMirrorRetry {
		if mirrorAt.IsZero() || !hasPoints {
			return fmt.Errorf("retry throttled")
		}
		// Keep serving the previous mirror copy until the retry window opens.
		return nil
	}

	s.mu.Lock()
	s.iersMirrorAttemptAt = now
	s.mu.Unlock()

	body, fileURL, fileName, err := s.resolveNTSCBulletinA(ctx)
	if err != nil {
		return err
	}
	points, err := parseDUT1Text(string(body))
	if err != nil {
		return fmt.Errorf("parse NTSC IERS mirror %s: %w", fileName, err)
	}

	s.mu.Lock()
	s.iersMirrorURL = fileURL
	s.iersMirrorName = fileName
	s.iersMirrorAt = time.Now().UTC()
	s.mu.Unlock()
	s.storePoints(points, "IERS Bulletin A (NTSC mirror)", fileURL)
	s.logger.Info("", "IERS Bulletin A loaded from NTSC mirror", map[string]interface{}{
		"file":   fileName,
		"source": fileURL,
		"days":   len(points),
	})
	return nil
}

var (
	ntscBulletinAProductPattern = regexp.MustCompile(`(?s)href="([^"]*erpFile[^"]*)"[^>]*>\s*Bulletin[ _-]?A\s*</a>`)
	ntscVolumeLinkPattern       = regexp.MustCompile(`(?s)href="([^"]*erpFile[^"]*)"[^>]*>\s*([0-9]{2}-volume-[^<]*?)\s*</a>`)
	ntscBulletinLinkPattern     = regexp.MustCompile(`href="(https?://[^"]*?/IERS/bulletina/[^"]*?/bulletina-[a-z]+-(\d{3})\.txt)"`)
)

// resolveNTSCBulletinA walks the NTSC IERS product pages and returns the newest
// Bulletin A file for the current year. The mirror URL is predictable only
// through these small listing pages, so resolve them instead of scraping a
// hard-coded file number.
func (s *dut1Service) resolveNTSCBulletinA(ctx context.Context) ([]byte, string, string, error) {
	listURL := s.ntscBaseURL + "/page/dataInternetList"
	listHTML, err := s.fetchHTML(ctx, listURL)
	if err != nil {
		return nil, "", "", fmt.Errorf("NTSC IERS product list: %w", err)
	}

	productHref := ""
	if match := ntscBulletinAProductPattern.FindStringSubmatch(string(listHTML)); len(match) >= 2 {
		productHref = strings.ReplaceAll(match[1], "&amp;", "&")
	}
	if productHref == "" {
		return nil, "", "", fmt.Errorf("no Bulletin A product entry found on NTSC IERS page")
	}
	productURL := absoluteNTSCPageURL(s.ntscBaseURL, productHref)
	productHTML, err := s.fetchHTML(ctx, productURL)
	if err != nil {
		return nil, "", "", fmt.Errorf("NTSC Bulletin A product page: %w", err)
	}

	bestVolume := -1
	volumeHref := ""
	for _, match := range ntscVolumeLinkPattern.FindAllStringSubmatch(string(productHTML), -1) {
		label := strings.TrimSpace(match[2])
		volumeNumber, err := strconv.Atoi(label[:2])
		if err != nil {
			continue
		}
		// Bulletin A volume numbers increase every year; the newest volume is
		// the highest number listed, not a year-label match.
		if volumeNumber > bestVolume {
			bestVolume = volumeNumber
			volumeHref = strings.ReplaceAll(match[1], "&amp;", "&")
		}
	}
	if volumeHref == "" {
		return nil, "", "", fmt.Errorf("no current IERS Bulletin A volume found")
	}
	volumeURL := absoluteNTSCPageURL(s.ntscBaseURL, volumeHref)

	volumeHTML, err := s.fetchHTML(ctx, volumeURL)
	if err != nil {
		return nil, "", "", fmt.Errorf("NTSC IERS volume page: %w", err)
	}
	bestIssue := -1
	bestFileURL := ""
	for _, match := range ntscBulletinLinkPattern.FindAllStringSubmatch(string(volumeHTML), -1) {
		issue, err := strconv.Atoi(match[2])
		if err != nil || issue <= bestIssue {
			continue
		}
		bestIssue = issue
		bestFileURL = strings.ReplaceAll(match[1], "&amp;", "&")
	}
	if bestFileURL == "" {
		return nil, "", "", fmt.Errorf("no Bulletin A file found in NTSC mirror volume page")
	}
	body, err := s.fetchSource(ctx, bestFileURL)
	if err != nil {
		return nil, "", "", fmt.Errorf("download NTSC IERS mirror: %w", err)
	}
	return body, bestFileURL, filepath.Base(bestFileURL), nil
}

func absoluteNTSCPageURL(baseURL, href string) string {
	raw := href
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if !parsed.IsAbs() {
		base, err := url.Parse(baseURL)
		if err != nil {
			return raw
		}
		parsed = base.ResolveReference(parsed)
	}
	// The NTSC product links contain raw Chinese in the query string. Re-encode
	// the parsed query so Go's HTTP client sends a valid request target.
	query := parsed.Query()
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func (s *dut1Service) fetchHTML(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "time-api/1.2 (DUT1 provider)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected HTTP status %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("empty response body")
	}
	return body, nil
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
		if service != nil {
			service.EnsureStarted()
		}
	case "iers":
		service = dut1
	default:
		// IERS is the primary product. Its provider falls back to the NTSC-hosted
		// IERS Bulletin A copy when the official host is unreachable. The NTSC
		// EOP product is an independent peer source, not an automatic backup, so
		// auto never substitutes it; clients can request source=ntsc explicitly.
		service = dut1
		if service == nil {
			service = dut1NTSC
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
