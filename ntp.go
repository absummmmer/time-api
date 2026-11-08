package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	ntpEpochOffset = 2208988800
	ntpPacketSize  = 48

	defaultNTPServer                  = "ntp.ntsc.ac.cn"
	defaultNTPInterval                = 5 * time.Minute
	defaultNTPTimeout                 = 3 * time.Second
	defaultNTPHoldoverAfter           = 20 * time.Minute
	defaultNTPMaxOffsetMs             = 10.0
	defaultNTPMaxUncertaintyMs        = 20.0
	defaultCorrectionMaxUncertaintyMs = 20.0
	defaultCorrectionMaxAge           = 10 * time.Minute
	defaultCorrectionMode             = "first"
	correctionModeOff                 = "off"
	correctionModeFirst               = "first"
	defaultCorrectionDriftMsPerHour   = 0.0
	minNTPInterval                    = 30 * time.Second
	minNTPTimeout                     = time.Second
	minNTPHoldoverAfter               = time.Minute
)

// NTPHealth is the read-only upstream clock health reported to clients.
// It is derived from an SNTP query made from this container; it never changes
// the host or container clock.
type NTPSourceHealth struct {
	Server              string  `json:"server"`
	ResolvedIP          string  `json:"resolved_ip,omitempty"`
	Reachable           bool    `json:"reachable"`
	Selected            bool    `json:"selected"`
	OffsetMs            float64 `json:"offset_ms"`
	UncertaintyMs       float64 `json:"uncertainty_ms"`
	RTTMs               float64 `json:"rtt_ms"`
	Stratum             int     `json:"stratum"`
	OffsetOk            bool    `json:"offset_ok"`
	QualityOk           bool    `json:"quality_ok"`
	RootDelayMs         float64 `json:"root_delay_ms"`
	RootDispersionMs    float64 `json:"root_dispersion_ms"`
	Leap                int     `json:"leap"`
	LastSuccessAt       string  `json:"last_success_at,omitempty"`
	LastAttemptAt       string  `json:"last_attempt_at,omitempty"`
	AgeSeconds          float64 `json:"age_s"`
	ConsecutiveFailures int     `json:"consecutive_failures"`
	Reach               int     `json:"reach"`
	ReachOctal          string  `json:"reach_octal"`
	Error               string  `json:"error,omitempty"`
}

type ntpSourceState struct {
	lastAttempt         time.Time
	lastSuccess         time.Time
	consecutiveFailures int
	reach               uint8
}

func (s *ntpSourceState) record(success bool, at time.Time) {
	s.lastAttempt = at
	s.reach = (s.reach << 1) & 0xFF
	if success {
		s.lastSuccess = at
		s.consecutiveFailures = 0
		s.reach |= 1
		return
	}
	s.consecutiveFailures++
}

type NTPHealth struct {
	Enabled          bool              `json:"enabled"`
	Server           string            `json:"server"`
	Servers          []string          `json:"servers,omitempty"`
	Sources          []NTPSourceHealth `json:"sources,omitempty"`
	Fallback         bool              `json:"fallback"`
	Reachable        bool              `json:"reachable"`
	Synchronized     bool              `json:"synchronized"`
	Holdover         bool              `json:"holdover"`
	HasSample        bool              `json:"has_sample"`
	OffsetOk         bool              `json:"offset_ok"`
	QualityOk        bool              `json:"quality_ok"`
	OffsetMs         float64           `json:"offset_ms"`
	UncertaintyMs    float64           `json:"uncertainty_ms"`
	RTTMs            float64           `json:"rtt_ms"`
	Stratum          int               `json:"stratum"`
	Leap             int               `json:"leap"`
	RefID            string            `json:"ref_id,omitempty"`
	RootDelayMs      float64           `json:"root_delay_ms"`
	RootDispersionMs float64           `json:"root_dispersion_ms"`
	LastSuccessAt    string            `json:"last_success_at,omitempty"`
	LastAttemptAt    string            `json:"last_attempt_at,omitempty"`
	ResolvedIP       string            `json:"resolved_ip,omitempty"`
	AgeSeconds       float64           `json:"age_s"`
	Error            string            `json:"error,omitempty"`
}

type ntpQueryResult struct {
	OffsetMs         float64
	RTTMs            float64
	UncertaintyMs    float64
	Stratum          int
	Leap             int
	RefID            string
	RootDelayMs      float64
	RootDispersionMs float64
	ResolvedIP       string
}

type ntpCandidate struct {
	server string
	index  int
	result ntpQueryResult
}

// selectNTPCandidate follows the configured priority order. It first prefers a
// source that passes the full trust gates, then a source whose uncertainty is
// good enough for best-effort correction, then the highest-priority reachable
// source. This lets a better fallback path drive the correction even when the
// primary host clock itself is outside the trust gates.
func selectNTPCandidate(candidates []ntpCandidate, maxOffsetMs, maxUncertaintyMs, correctionMaxUncertaintyMs float64) *ntpCandidate {
	var firstReachable *ntpCandidate
	var firstCorrectable *ntpCandidate
	for i := range candidates {
		candidate := &candidates[i]
		if firstReachable == nil {
			firstReachable = candidate
		}
		if firstCorrectable == nil && candidate.result.UncertaintyMs <= correctionMaxUncertaintyMs {
			firstCorrectable = candidate
		}
		if math.Abs(candidate.result.OffsetMs) <= maxOffsetMs &&
			candidate.result.UncertaintyMs <= maxUncertaintyMs {
			return candidate
		}
	}
	if firstCorrectable != nil {
		return firstCorrectable
	}
	return firstReachable
}

// NTPMonitor periodically samples the configured upstream NTP server and
// caches the result. It is deliberately observe-only.
type NTPMonitor struct {
	mu                         sync.RWMutex
	queryMu                    sync.Mutex
	enabled                    bool
	server                     string
	servers                    []string
	fallback                   bool
	interval                   time.Duration
	timeout                    time.Duration
	holdoverAfter              time.Duration
	maxOffsetMs                float64
	maxUncertaintyMs           float64
	correctionMaxUncertaintyMs float64
	correctionMaxAge           time.Duration
	correctionMode             string
	correctionDriftMsPerHour   float64

	reachable   bool
	lastAttempt time.Time
	lastSuccess time.Time
	lastError   string

	offsetMs         float64
	rttMs            float64
	uncertaintyMs    float64
	resolvedIP       string
	sources          []NTPSourceHealth
	sourceStates     map[string]*ntpSourceState
	stratum          int
	leap             int
	refID            string
	rootDelayMs      float64
	rootDispersionMs float64
}

var ntpMonitor *NTPMonitor
var ntpTimeSequence uint64

const ntpProbeMinInterval = 2 * time.Second

var ntpProbeMu sync.Mutex
var ntpProbeLast = make(map[string]time.Time)

// NTPTimeResponse is the compact, NTP-oriented time transfer response.
// Unix microseconds are integers below JavaScript's Number.MAX_SAFE_INTEGER,
// so browser clients can parse them without losing precision.
type NTPTimeResponse struct {
	Sequence                     uint64    `json:"sequence"`
	ServerReceivedUnixUs         int64     `json:"server_received_unix_us"`
	ServerSentUnixUs             int64     `json:"server_sent_unix_us"`
	ServerReceivedMs             int64     `json:"server_received_ms"`
	ServerSentMs                 int64     `json:"server_sent_ms"`
	HostReceivedUnixUs           int64     `json:"host_received_unix_us"`
	HostSentUnixUs               int64     `json:"host_sent_unix_us"`
	HostReceivedMs               int64     `json:"host_received_ms"`
	HostSentMs                   int64     `json:"host_sent_ms"`
	ClockCorrectionMs            float64   `json:"clock_correction_ms"`
	ClockCorrectionApplied       bool      `json:"clock_correction_applied"`
	ClockCorrectionEligible      bool      `json:"clock_correction_eligible"`
	ClockCorrectionMode          string    `json:"clock_correction_mode"`
	ClockCorrectionSource        string    `json:"clock_correction_source,omitempty"`
	ClockCorrectionReason        string    `json:"clock_correction_reason,omitempty"`
	ClockCorrectionUncertaintyMs float64   `json:"clock_correction_uncertainty_ms"`
	ClockSource                  string    `json:"clock_source"`
	Date                         string    `json:"date"`
	Time                         string    `json:"time"`
	Probe                        bool      `json:"probe,omitempty"`
	Refreshed                    bool      `json:"refreshed,omitempty"`
	ClockHealth                  NTPHealth `json:"clock_health"`
}

func ntpProbeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store, max-age=0")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func ntpProbeAllowed(clientIP string) bool {
	clientIP = strings.TrimSpace(clientIP)
	if host, _, err := net.SplitHostPort(clientIP); err == nil {
		clientIP = host
	}
	if clientIP == "" {
		clientIP = "unknown"
	}
	now := time.Now()
	ntpProbeMu.Lock()
	defer ntpProbeMu.Unlock()
	if last, ok := ntpProbeLast[clientIP]; ok && now.Sub(last) < ntpProbeMinInterval {
		return false
	}
	ntpProbeLast[clientIP] = now
	if len(ntpProbeLast) > 2048 {
		for key, at := range ntpProbeLast {
			if now.Sub(at) > 10*time.Minute {
				delete(ntpProbeLast, key)
			}
		}
	}
	return true
}

func normalizeNTPProbeServer(raw string) (string, error) {
	server := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
	if server == "" || len(server) > 253 {
		return "", fmt.Errorf("invalid NTP server name")
	}
	if net.ParseIP(server) != nil {
		return "", fmt.Errorf("NTP probe requires a DNS name, not an IP literal")
	}
	if strings.ContainsAny(server, "/\\:@?#[] ") {
		return "", fmt.Errorf("invalid NTP server name")
	}
	if server == "localhost" || strings.HasSuffix(server, ".local") ||
		strings.HasSuffix(server, ".lan") || strings.HasSuffix(server, ".internal") ||
		strings.HasSuffix(server, ".home") {
		return "", fmt.Errorf("private/local NTP hostnames are not allowed")
	}
	labels := strings.Split(server, ".")
	if len(labels) < 2 {
		return "", fmt.Errorf("NTP server must be a fully qualified DNS name")
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 {
			return "", fmt.Errorf("invalid NTP server name")
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("invalid NTP server name")
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return "", fmt.Errorf("invalid NTP server name")
			}
		}
	}
	tld := labels[len(labels)-1]
	if len(tld) < 2 || strings.Contains(tld, "-") {
		return "", fmt.Errorf("invalid NTP server name")
	}
	for _, char := range tld {
		if char < 'a' || char > 'z' {
			return "", fmt.Errorf("invalid NTP server name")
		}
	}
	ips, err := net.LookupIP(server)
	if err != nil {
		return "", fmt.Errorf("cannot resolve NTP server: %w", err)
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsMulticast() ||
			ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			return "", fmt.Errorf("NTP server resolves to a non-public address")
		}
	}
	if len(ips) == 0 {
		return "", fmt.Errorf("NTP server has no address")
	}
	return server, nil
}

func probeNTPHealth(server string, result ntpQueryResult, err error, maxOffsetMs, maxUncertaintyMs float64) NTPHealth {
	now := time.Now().UTC()
	health := NTPHealth{
		Enabled:       true,
		Server:        server,
		LastAttemptAt: now.Format(time.RFC3339Nano),
	}
	if err != nil {
		health.Error = err.Error()
		health.Sources = []NTPSourceHealth{{
			Server:              server,
			Error:               err.Error(),
			LastAttemptAt:       now.Format(time.RFC3339Nano),
			ConsecutiveFailures: 1,
			Reach:               0,
			ReachOctal:          "000",
		}}
		return health
	}
	health.Reachable = true
	health.HasSample = true
	health.OffsetMs = result.OffsetMs
	health.RTTMs = result.RTTMs
	health.UncertaintyMs = result.UncertaintyMs
	health.Stratum = result.Stratum
	health.Leap = result.Leap
	health.RefID = result.RefID
	health.RootDelayMs = result.RootDelayMs
	health.RootDispersionMs = result.RootDispersionMs
	health.ResolvedIP = result.ResolvedIP
	health.OffsetOk = math.Abs(result.OffsetMs) <= maxOffsetMs
	health.QualityOk = result.UncertaintyMs <= maxUncertaintyMs
	health.Synchronized = health.OffsetOk && health.QualityOk
	health.LastSuccessAt = now.Format(time.RFC3339Nano)
	health.Sources = []NTPSourceHealth{{
		Server:           server,
		ResolvedIP:       result.ResolvedIP,
		Reachable:        true,
		Selected:         true,
		OffsetMs:         result.OffsetMs,
		UncertaintyMs:    result.UncertaintyMs,
		RTTMs:            result.RTTMs,
		Stratum:          result.Stratum,
		OffsetOk:         math.Abs(result.OffsetMs) <= maxOffsetMs,
		QualityOk:        result.UncertaintyMs <= maxUncertaintyMs,
		RootDelayMs:      result.RootDelayMs,
		RootDispersionMs: result.RootDispersionMs,
		Leap:             result.Leap,
		LastSuccessAt:    now.Format(time.RFC3339Nano),
		LastAttemptAt:    now.Format(time.RFC3339Nano),
		Reach:            1,
		ReachOctal:       "001",
	}}
	return health
}

func maxNTPOffsetFromMonitor() float64 {
	if ntpMonitor != nil && ntpMonitor.maxOffsetMs > 0 {
		return ntpMonitor.maxOffsetMs
	}
	return defaultNTPMaxOffsetMs
}

func maxNTPUncertaintyFromMonitor() float64 {
	if ntpMonitor != nil && ntpMonitor.maxUncertaintyMs > 0 {
		return ntpMonitor.maxUncertaintyMs
	}
	return defaultNTPMaxUncertaintyMs
}

func correctionMaxUncertainty() float64 {
	if ntpMonitor != nil && ntpMonitor.correctionMaxUncertaintyMs > 0 {
		return ntpMonitor.correctionMaxUncertaintyMs
	}
	return defaultCorrectionMaxUncertaintyMs
}

func correctionMaxAge() time.Duration {
	if ntpMonitor != nil && ntpMonitor.correctionMaxAge > 0 {
		return ntpMonitor.correctionMaxAge
	}
	return defaultCorrectionMaxAge
}

func correctionMode() string {
	if ntpMonitor != nil && ntpMonitor.correctionMode != "" {
		return ntpMonitor.correctionMode
	}
	return defaultCorrectionMode
}

func correctionDriftMsPerHour() float64 {
	if ntpMonitor != nil && ntpMonitor.correctionDriftMsPerHour > 0 {
		return ntpMonitor.correctionDriftMsPerHour
	}
	return defaultCorrectionDriftMsPerHour
}

type ntpCorrectionDecision struct {
	Ms            float64
	Applied       bool
	Eligible      bool
	Mode          string
	Source        string
	Reason        string
	UncertaintyMs float64
}

// effectiveCorrectionUncertainty grows the sample uncertainty with age. The
// default drift budget is zero; the field is opt-in until field data exists.
func effectiveCorrectionUncertainty(health NTPHealth) float64 {
	uncertainty := health.UncertaintyMs
	if health.AgeSeconds > 0 {
		uncertainty += health.AgeSeconds / 3600 * correctionDriftMsPerHour()
	}
	return uncertainty
}

// decideNTPServerCorrection separates "is this source good enough to report"
// from "may it modify served timestamps". In first mode only the first
// configured source, while fully synchronized and not in fallback, may correct.
// Other sources are observation-only. The server name is deliberately not
// hard-coded: the first configured entry is the authority.
func decideNTPServerCorrection(health NTPHealth, mode string) ntpCorrectionDecision {
	decision := ntpCorrectionDecision{
		Mode:   mode,
		Source: health.Server,
	}
	if mode == "" {
		mode = defaultCorrectionMode
		decision.Mode = mode
	}
	decision.UncertaintyMs = effectiveCorrectionUncertainty(health)
	if mode == correctionModeOff {
		decision.Reason = "mode_off"
		return decision
	}
	if mode != correctionModeFirst {
		decision.Reason = "invalid_mode"
		return decision
	}
	if !health.Enabled || !health.HasSample || !health.Reachable {
		decision.Reason = "unavailable"
		return decision
	}
	if health.Fallback {
		decision.Reason = "fallback_source"
		return decision
	}
	if health.Holdover {
		decision.Reason = "holdover"
		return decision
	}
	if !health.OffsetOk {
		decision.Reason = "offset_gate"
		return decision
	}
	if !health.QualityOk {
		decision.Reason = "quality_gate"
		return decision
	}
	if math.IsNaN(health.OffsetMs) || math.IsInf(health.OffsetMs, 0) ||
		math.IsNaN(decision.UncertaintyMs) || math.IsInf(decision.UncertaintyMs, 0) {
		decision.Reason = "invalid_value"
		return decision
	}
	if decision.UncertaintyMs > correctionMaxUncertainty() {
		decision.Reason = "uncertainty_gate"
		return decision
	}
	if health.AgeSeconds > correctionMaxAge().Seconds() {
		decision.Reason = "age_gate"
		return decision
	}
	decision.Eligible = true
	decision.Applied = true
	decision.Ms = health.OffsetMs
	decision.Reason = "primary_synchronized"
	return decision
}

// correctionForHealth keeps the old two-value helper for the handler tests.
func correctionForHealth(health NTPHealth) (float64, bool) {
	decision := decideNTPServerCorrection(health, correctionMode())
	return decision.Ms, decision.Applied
}

func ntpProbeTimeout() time.Duration {
	timeout := defaultNTPTimeout
	if ntpMonitor != nil && ntpMonitor.timeout > 0 {
		timeout = ntpMonitor.timeout
	}
	if timeout > 3*time.Second {
		timeout = 3 * time.Second
	}
	return timeout
}

func ntpTimeV1Handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store, max-age=0")
	probeServer := strings.TrimSpace(r.URL.Query().Get("server"))
	probe := probeServer != ""
	refresh := false
	if rawRefresh := strings.TrimSpace(r.URL.Query().Get("refresh")); rawRefresh != "" {
		parsed, err := strconv.ParseBool(rawRefresh)
		if err != nil {
			ntpProbeError(w, http.StatusBadRequest, "invalid refresh value")
			return
		}
		refresh = parsed
	}
	if probe && refresh {
		ntpProbeError(w, http.StatusBadRequest, "use either server or refresh, not both")
		return
	}
	var health NTPHealth

	if probe {
		if !ntpProbeAllowed(getClientIP(r)) {
			ntpProbeError(w, http.StatusTooManyRequests, "NTP query rate limit exceeded")
			return
		}
		server, err := normalizeNTPProbeServer(probeServer)
		if err != nil {
			ntpProbeError(w, http.StatusBadRequest, err.Error())
			return
		}
		result, queryErr := queryNTPServer(server, ntpProbeTimeout())
		health = probeNTPHealth(server, result, queryErr, maxNTPOffsetFromMonitor(), maxNTPUncertaintyFromMonitor())
	} else if refresh {
		if !ntpProbeAllowed(getClientIP(r)) {
			ntpProbeError(w, http.StatusTooManyRequests, "NTP query rate limit exceeded")
			return
		}
		if ntpMonitor == nil || !ntpMonitor.enabled {
			ntpProbeError(w, http.StatusServiceUnavailable, "NTP monitor disabled")
			return
		}
		health = ntpMonitor.RefreshNow()
	} else {
		health = NTPHealth{Server: defaultNTPServer, Error: "NTP monitor unavailable"}
		if ntpMonitor != nil {
			health = ntpMonitor.Status()
		}
	}

	hostReceived := time.Now().UTC()
	hostSent := time.Now().UTC()
	decision := decideNTPServerCorrection(health, correctionMode())
	if probe {
		// A ?server= request is diagnostic only. It must not apply a correction
		// from a source that is not the configured first source.
		decision.Applied = false
		decision.Eligible = false
		decision.Ms = 0
		decision.Reason = "probe_only"
	}
	correctionMs := decision.Ms
	correctionApplied := decision.Applied
	correctionUs := int64(0)
	if correctionApplied {
		correctionUs = int64(math.Round(correctionMs * 1000))
	}
	serverReceivedUs := hostReceived.UnixMicro() + correctionUs
	serverSentUs := hostSent.UnixMicro() + correctionUs
	serverDate := time.UnixMicro(serverReceivedUs).UTC()
	clockSource := "host"
	if correctionApplied {
		clockSource = "host+first-source"
	}
	response := NTPTimeResponse{
		Sequence:                     atomic.AddUint64(&ntpTimeSequence, 1),
		ServerReceivedUnixUs:         serverReceivedUs,
		ServerSentUnixUs:             serverSentUs,
		ServerReceivedMs:             serverReceivedUs / 1000,
		ServerSentMs:                 serverSentUs / 1000,
		HostReceivedUnixUs:           hostReceived.UnixMicro(),
		HostSentUnixUs:               hostSent.UnixMicro(),
		HostReceivedMs:               hostReceived.UnixMilli(),
		HostSentMs:                   hostSent.UnixMilli(),
		ClockCorrectionMs:            correctionMs,
		ClockCorrectionApplied:       correctionApplied,
		ClockCorrectionEligible:      decision.Eligible,
		ClockCorrectionMode:          decision.Mode,
		ClockCorrectionSource:        decision.Source,
		ClockCorrectionReason:        decision.Reason,
		ClockCorrectionUncertaintyMs: decision.UncertaintyMs,
		ClockSource:                  clockSource,
		Date:                         serverDate.Format("2006-01-02"),
		Time:                         serverDate.Format("15:04:05.999999"),
		Probe:                        probe,
		Refreshed:                    refresh,
		ClockHealth:                  health,
	}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		Logger.Error("", "NTP time response encode failed", map[string]any{"error": err.Error()})
	}
}

func envBool(name string, fallback bool) bool {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on", "enabled":
		return true
	case "0", "false", "no", "off", "disabled":
		return false
	default:
		return fallback
	}
}

func envMonitorDuration(name string, fallback, minimum time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value < minimum {
		return fallback
	}
	return value
}

func envFloat(name string, fallback float64) float64 {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return fallback
	}
	return value
}

func envCorrectionMode(name, fallback string) string {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	switch value {
	case correctionModeOff, correctionModeFirst:
		return value
	default:
		return fallback
	}
}

func parseNTPServers(raw string) []string {
	parts := strings.Split(raw, ",")
	servers := make([]string, 0, len(parts))
	seen := make(map[string]bool)
	for _, part := range parts {
		server := strings.TrimSpace(part)
		if server == "" || seen[server] {
			continue
		}
		seen[server] = true
		servers = append(servers, server)
		if len(servers) >= 8 {
			break
		}
	}
	if len(servers) == 0 {
		return []string{defaultNTPServer}
	}
	return servers
}

func newNTPMonitorFromEnv() *NTPMonitor {
	serverList := strings.TrimSpace(os.Getenv("NTP_MONITOR_SERVERS"))
	if serverList == "" {
		serverList = strings.TrimSpace(os.Getenv("NTP_MONITOR_SERVER"))
	}
	servers := parseNTPServers(serverList)
	interval := envMonitorDuration("NTP_MONITOR_INTERVAL", defaultNTPInterval, minNTPInterval)
	timeout := envMonitorDuration("NTP_MONITOR_TIMEOUT", defaultNTPTimeout, minNTPTimeout)
	defaultHoldover := 3 * interval
	if defaultHoldover < defaultNTPHoldoverAfter {
		defaultHoldover = defaultNTPHoldoverAfter
	}
	holdoverAfter := envMonitorDuration("NTP_MONITOR_HOLDOVER_AFTER", defaultHoldover, minNTPHoldoverAfter)
	monitor := &NTPMonitor{
		enabled:                    envBool("NTP_MONITOR_ENABLED", true),
		server:                     servers[0],
		servers:                    servers,
		interval:                   interval,
		timeout:                    timeout,
		holdoverAfter:              holdoverAfter,
		maxOffsetMs:                envFloat("NTP_MONITOR_MAX_OFFSET_MS", defaultNTPMaxOffsetMs),
		maxUncertaintyMs:           envFloat("NTP_MONITOR_MAX_UNCERTAINTY_MS", defaultNTPMaxUncertaintyMs),
		correctionMaxUncertaintyMs: envFloat("NTP_CORRECTION_MAX_UNCERTAINTY_MS", defaultCorrectionMaxUncertaintyMs),
		correctionMaxAge:           envMonitorDuration("NTP_CORRECTION_MAX_AGE", defaultCorrectionMaxAge, time.Minute),
		correctionMode:             envCorrectionMode("NTP_CORRECTION_MODE", defaultCorrectionMode),
		correctionDriftMsPerHour:   envFloat("NTP_CORRECTION_DRIFT_MS_PER_HOUR", defaultCorrectionDriftMsPerHour),
		sourceStates:               make(map[string]*ntpSourceState, len(servers)),
	}
	for _, server := range servers {
		monitor.sourceStates[server] = &ntpSourceState{}
	}
	return monitor
}

func (m *NTPMonitor) Start(ctx context.Context) {
	if !m.enabled {
		Logger.Info("", "NTP monitor disabled", map[string]any{"servers": m.servers})
		return
	}
	Logger.Info("", "NTP monitor starting", map[string]any{
		"servers":        m.servers,
		"interval":       m.interval.String(),
		"timeout":        m.timeout.String(),
		"holdover_after": m.holdoverAfter.String(),
		"correction":     m.correctionMode,
	})
	go func() {
		m.runOnce()
		ticker := time.NewTicker(m.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.runOnce()
			}
		}
	}()
}

type ntpServerOutcome struct {
	index  int
	server string
	result ntpQueryResult
	err    error
}

func (m *NTPMonitor) queryAll() []ntpServerOutcome {
	ch := make(chan ntpServerOutcome, len(m.servers))
	var wg sync.WaitGroup
	for index, server := range m.servers {
		wg.Add(1)
		go func(index int, server string) {
			defer wg.Done()
			result, err := queryNTPServer(server, m.timeout)
			ch <- ntpServerOutcome{index: index, server: server, result: result, err: err}
		}(index, server)
	}
	go func() {
		wg.Wait()
		close(ch)
	}()
	outcomes := make([]ntpServerOutcome, 0, len(m.servers))
	for outcome := range ch {
		outcomes = append(outcomes, outcome)
	}
	sort.Slice(outcomes, func(i, j int) bool { return outcomes[i].index < outcomes[j].index })
	return outcomes
}

func (m *NTPMonitor) runOnce() {
	m.queryMu.Lock()
	defer m.queryMu.Unlock()
	attempt := time.Now().UTC()

	outcomes := m.queryAll()
	candidates := make([]ntpCandidate, 0, len(outcomes))
	sources := make([]NTPSourceHealth, 0, len(outcomes))
	failures := make([]string, 0, len(outcomes))
	for _, outcome := range outcomes {
		source := NTPSourceHealth{Server: outcome.server}
		if outcome.err != nil {
			source.Error = outcome.err.Error()
			failures = append(failures, fmt.Sprintf("%s: %v", outcome.server, outcome.err))
			Logger.Error("", "NTP monitor query failed", map[string]any{
				"server": outcome.server,
				"error":  outcome.err.Error(),
			})
		} else {
			source.Reachable = true
			source.ResolvedIP = outcome.result.ResolvedIP
			source.OffsetMs = outcome.result.OffsetMs
			source.UncertaintyMs = outcome.result.UncertaintyMs
			source.RTTMs = outcome.result.RTTMs
			source.Stratum = outcome.result.Stratum
			source.OffsetOk = math.Abs(outcome.result.OffsetMs) <= m.maxOffsetMs
			source.QualityOk = outcome.result.UncertaintyMs <= m.maxUncertaintyMs
			source.RootDelayMs = outcome.result.RootDelayMs
			source.RootDispersionMs = outcome.result.RootDispersionMs
			source.Leap = outcome.result.Leap
			candidates = append(candidates, ntpCandidate{
				server: outcome.server,
				index:  outcome.index,
				result: outcome.result,
			})
		}
		sources = append(sources, source)
	}
	selected := selectNTPCandidate(candidates, m.maxOffsetMs, m.maxUncertaintyMs, m.correctionMaxUncertaintyMs)
	if selected != nil {
		for index := range sources {
			sources[index].Selected = index == selected.index
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastAttempt = attempt
	if m.sourceStates == nil {
		m.sourceStates = make(map[string]*ntpSourceState, len(sources))
	}
	for index := range sources {
		state := m.sourceStates[sources[index].Server]
		if state == nil {
			state = &ntpSourceState{}
			m.sourceStates[sources[index].Server] = state
		}
		state.record(sources[index].Reachable, attempt)
		sources[index].ConsecutiveFailures = state.consecutiveFailures
		sources[index].Reach = int(state.reach)
		sources[index].ReachOctal = fmt.Sprintf("%03o", state.reach)
		sources[index].LastAttemptAt = state.lastAttempt.Format(time.RFC3339Nano)
		if !state.lastSuccess.IsZero() {
			sources[index].LastSuccessAt = state.lastSuccess.Format(time.RFC3339Nano)
		}
	}
	m.sources = sources
	if selected == nil {
		m.reachable = false
		m.lastError = fmt.Sprintf("all NTP sources failed: %s", strings.Join(failures, "; "))
		return
	}

	result := selected.result
	m.reachable = true
	m.lastSuccess = attempt
	m.lastError = ""
	m.server = selected.server
	m.fallback = selected.index > 0
	m.offsetMs = result.OffsetMs
	m.rttMs = result.RTTMs
	m.uncertaintyMs = result.UncertaintyMs
	m.resolvedIP = result.ResolvedIP
	m.stratum = result.Stratum
	m.leap = result.Leap
	m.refID = result.RefID
	m.rootDelayMs = result.RootDelayMs
	m.rootDispersionMs = result.RootDispersionMs
	Logger.Info("", "NTP monitor query ok", map[string]any{
		"server":         m.server,
		"fallback":       m.fallback,
		"offset_ms":      result.OffsetMs,
		"uncertainty_ms": result.UncertaintyMs,
		"rtt_ms":         result.RTTMs,
		"stratum":        result.Stratum,
		"leap":           result.Leap,
		"ref_id":         result.RefID,
	})
}

// RefreshNow performs one synchronous query against the configured monitor
// source and returns the resulting cached state. It is used by the explicit
// ?refresh=1 diagnostic path; the normal background schedule is unchanged.
func (m *NTPMonitor) RefreshNow() NTPHealth {
	if m == nil {
		return NTPHealth{Error: "NTP monitor unavailable"}
	}
	if !m.enabled {
		return m.Status()
	}
	m.runOnce()
	return m.Status()
}

// Status returns a copy of the cached upstream NTP state.
func (m *NTPMonitor) Status() NTPHealth {
	m.mu.RLock()
	defer m.mu.RUnlock()

	now := time.Now().UTC()
	sources := append([]NTPSourceHealth(nil), m.sources...)
	for index := range sources {
		state := m.sourceStates[sources[index].Server]
		if state == nil {
			continue
		}
		sources[index].ConsecutiveFailures = state.consecutiveFailures
		sources[index].Reach = int(state.reach)
		sources[index].ReachOctal = fmt.Sprintf("%03o", state.reach)
		if !state.lastAttempt.IsZero() {
			sources[index].LastAttemptAt = state.lastAttempt.Format(time.RFC3339Nano)
		}
		if !state.lastSuccess.IsZero() {
			sources[index].LastSuccessAt = state.lastSuccess.Format(time.RFC3339Nano)
			sources[index].AgeSeconds = now.Sub(state.lastSuccess).Seconds()
		}
	}
	health := NTPHealth{
		Enabled:          m.enabled,
		Server:           m.server,
		Servers:          append([]string(nil), m.servers...),
		Sources:          sources,
		Fallback:         m.fallback,
		Reachable:        m.reachable,
		HasSample:        !m.lastSuccess.IsZero(),
		OffsetMs:         m.offsetMs,
		RTTMs:            m.rttMs,
		ResolvedIP:       m.resolvedIP,
		Stratum:          m.stratum,
		Leap:             m.leap,
		RefID:            m.refID,
		RootDelayMs:      m.rootDelayMs,
		RootDispersionMs: m.rootDispersionMs,
		Error:            m.lastError,
	}
	if health.HasSample {
		health.LastSuccessAt = m.lastSuccess.Format(time.RFC3339Nano)
		health.AgeSeconds = now.Sub(m.lastSuccess).Seconds()
		health.Holdover = health.AgeSeconds > m.holdoverAfter.Seconds()
		health.UncertaintyMs = m.uncertaintyMs
		health.OffsetOk = math.Abs(m.offsetMs) <= m.maxOffsetMs
		health.QualityOk = m.uncertaintyMs <= m.maxUncertaintyMs
		health.Synchronized = m.reachable && !health.Holdover && health.OffsetOk && health.QualityOk
	}
	if !m.lastAttempt.IsZero() {
		health.LastAttemptAt = m.lastAttempt.Format(time.RFC3339Nano)
	}
	if !health.HasSample && health.Error == "" {
		health.Error = "waiting for first NTP sample"
	}
	return health
}

func ntpAddress(server string) string {
	if _, _, err := net.SplitHostPort(server); err == nil {
		return server
	}
	return net.JoinHostPort(server, "123")
}

// ntpEndpoints expands a configured server name into all of its IP addresses.
// DNS names such as ntp.ntsc.ac.cn are load-balanced; querying only the first
// A record can mark the whole source unreachable even when another address is
// healthy.
func ntpEndpoints(server string) ([]string, error) {
	host := server
	port := "123"
	if parsedHost, parsedPort, err := net.SplitHostPort(server); err == nil {
		host = parsedHost
		port = parsedPort
	}
	if ip := net.ParseIP(host); ip != nil {
		return []string{net.JoinHostPort(host, port)}, nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	seen := make(map[string]bool, len(ips))
	endpoints := make([]string, 0, len(ips))
	for _, ip := range ips {
		address := net.JoinHostPort(ip.String(), port)
		if seen[address] {
			continue
		}
		seen[address] = true
		endpoints = append(endpoints, address)
	}
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", host)
	}
	return endpoints, nil
}

func betterNTPResult(candidate, current ntpQueryResult) bool {
	if candidate.UncertaintyMs < current.UncertaintyMs {
		return true
	}
	if candidate.UncertaintyMs == current.UncertaintyMs && candidate.RTTMs < current.RTTMs {
		return true
	}
	return false
}

type ntpAddressOutcome struct {
	address string
	result  ntpQueryResult
	err     error
}

// queryNTPServer tries every address returned for a configured server and
// picks the observation with the lowest uncertainty (then lowest RTT). It is a
// probe-only operation and never changes the host clock.
func queryNTPServer(server string, timeout time.Duration) (ntpQueryResult, error) {
	endpoints, err := ntpEndpoints(server)
	if err != nil {
		return ntpQueryResult{}, err
	}
	outcomes := make(chan ntpAddressOutcome, len(endpoints))
	for _, address := range endpoints {
		go func(address string) {
			result, queryErr := queryNTPServerAddress(address, timeout)
			outcomes <- ntpAddressOutcome{address: address, result: result, err: queryErr}
		}(address)
	}
	var best ntpQueryResult
	found := false
	failures := make([]string, 0, len(endpoints))
	for range endpoints {
		outcome := <-outcomes
		if outcome.err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", outcome.address, outcome.err))
			continue
		}
		if !found || betterNTPResult(outcome.result, best) {
			best = outcome.result
			found = true
		}
	}
	if !found {
		return ntpQueryResult{}, fmt.Errorf("all addresses failed for %s: %s", server, strings.Join(failures, "; "))
	}
	return best, nil
}

func queryNTPServerAddress(address string, timeout time.Duration) (ntpQueryResult, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return ntpQueryResult{}, fmt.Errorf("split address %s: %w", address, err)
	}
	network := "udp4"
	if parsedIP := net.ParseIP(host); parsedIP != nil && parsedIP.To4() == nil {
		network = "udp6"
	}
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.Dial(network, address)
	if err != nil {
		return ntpQueryResult{}, fmt.Errorf("dial %s: %w", address, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return ntpQueryResult{}, fmt.Errorf("set deadline: %w", err)
	}

	request := make([]byte, ntpPacketSize)
	// LI=0, VN=3, Mode=3 (client).
	request[0] = 0x1B
	t1 := time.Now()
	writeNTPTimestamp(request[40:48], t1)
	if _, err := conn.Write(request); err != nil {
		return ntpQueryResult{}, fmt.Errorf("send NTP request: %w", err)
	}

	response := make([]byte, 512)
	n, err := conn.Read(response)
	t4 := time.Now()
	if err != nil {
		return ntpQueryResult{}, fmt.Errorf("read NTP response: %w", err)
	}
	if n < ntpPacketSize {
		return ntpQueryResult{}, fmt.Errorf("short NTP response: %d bytes", n)
	}
	result, err := parseNTPResponse(response[:n], t1, t4)
	if err != nil {
		return ntpQueryResult{}, err
	}
	result.ResolvedIP = host
	return result, nil
}

func parseNTPResponse(response []byte, t1, t4 time.Time) (ntpQueryResult, error) {
	if len(response) < ntpPacketSize {
		return ntpQueryResult{}, fmt.Errorf("short NTP response: %d bytes", len(response))
	}
	mode := response[0] & 0x07
	if mode != 4 && mode != 5 {
		return ntpQueryResult{}, fmt.Errorf("unexpected NTP mode %d", mode)
	}
	stratum := int(response[1])
	if stratum == 0 {
		return ntpQueryResult{}, fmt.Errorf("NTP kiss-o'-death received")
	}
	if stratum >= 16 {
		return ntpQueryResult{}, fmt.Errorf("NTP server unsynchronized: stratum %d", stratum)
	}
	leap := int(response[0] >> 6)
	if leap == 3 {
		return ntpQueryResult{}, fmt.Errorf("NTP server reports clock not synchronized")
	}

	t2, ok2 := readNTPTimestamp(response[32:40])
	if !ok2 {
		return ntpQueryResult{}, fmt.Errorf("missing NTP receive timestamp")
	}
	t3, ok3 := readNTPTimestamp(response[40:48])
	if !ok3 {
		return ntpQueryResult{}, fmt.Errorf("missing NTP transmit timestamp")
	}

	offset := (t2.Sub(t1) + t3.Sub(t4)).Seconds() * 1000 / 2
	delay := (t4.Sub(t1) - (t3.Sub(t2))).Seconds() * 1000
	if delay < 0 {
		delay = 0
	}
	rootDelayMs := fixed16_16(response[4:8]) * 1000
	rootDispersionMs := fixed16_16(response[8:12]) * 1000
	result := ntpQueryResult{
		OffsetMs:         offset,
		RTTMs:            delay,
		UncertaintyMs:    ntpUncertaintyMs(delay, rootDelayMs, rootDispersionMs),
		Stratum:          stratum,
		Leap:             leap,
		RefID:            ntpRefID(response[12:16], stratum),
		RootDelayMs:      rootDelayMs,
		RootDispersionMs: rootDispersionMs,
	}
	return result, nil
}

// ntpUncertaintyMs is a conservative heuristic: it takes the larger of the
// network round-trip bound and the NTP root-distance term. It is not a formal
// confidence interval.
func ntpUncertaintyMs(rttMs, rootDelayMs, rootDispersionMs float64) float64 {
	uncertainty := rttMs / 2
	rootDistance := rootDelayMs/2 + rootDispersionMs
	if rootDistance > uncertainty {
		uncertainty = rootDistance
	}
	if uncertainty < 0 {
		return 0
	}
	return uncertainty
}

func ntpRefID(raw []byte, stratum int) string {
	if len(raw) < 4 {
		return ""
	}
	if stratum <= 1 {
		return fmt.Sprintf("%d.%d.%d.%d", raw[0], raw[1], raw[2], raw[3])
	}
	value := strings.TrimRight(string(raw), "\x00")
	if value == "" {
		return ""
	}
	for _, char := range []byte(value) {
		if char < 32 || char > 126 {
			// Some servers put a binary reference ID in stratum >= 2.
			return fmt.Sprintf("0x%X", raw)
		}
	}
	return strings.TrimSpace(value)
}

func fixed16_16(raw []byte) float64 {
	if len(raw) < 4 {
		return 0
	}
	return float64(binary.BigEndian.Uint32(raw)) / 65536
}

func readNTPTimestamp(raw []byte) (time.Time, bool) {
	if len(raw) < 8 {
		return time.Time{}, false
	}
	seconds := int64(binary.BigEndian.Uint32(raw[0:4]))
	fraction := binary.BigEndian.Uint32(raw[4:8])
	if seconds == 0 && fraction == 0 {
		return time.Time{}, false
	}
	if seconds < ntpEpochOffset {
		// NTP era 1 wraps in 2036. This also keeps a recently reset server
		// from being interpreted as a date in 1900.
		seconds += 1 << 32
	}
	unixSeconds := seconds - ntpEpochOffset
	nanos := int64((uint64(fraction) * 1_000_000_000) >> 32)
	return time.Unix(unixSeconds, nanos).UTC(), true
}

func writeNTPTimestamp(raw []byte, t time.Time) {
	if len(raw) < 8 {
		return
	}
	unixSeconds := t.Unix()
	seconds := uint32(unixSeconds + ntpEpochOffset)
	fraction := uint32((uint64(t.Nanosecond()) << 32) / 1_000_000_000)
	binary.BigEndian.PutUint32(raw[0:4], seconds)
	binary.BigEndian.PutUint32(raw[4:8], fraction)
}
