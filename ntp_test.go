package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http/httptest"
	"testing"
	"time"
)

func makeNTPResponse(receive, transmit time.Time, stratum int, leap int) []byte {
	packet := make([]byte, ntpPacketSize)
	packet[0] = byte((leap & 3) << 6)
	packet[0] |= 4 << 3 // version 4
	packet[0] |= 4      // server mode
	packet[1] = byte(stratum)
	packet[2] = 6
	packet[3] = 0xEC // -20, approximately
	// root delay 0.010 s, root dispersion 0.002 s
	packet[4] = 0
	packet[5] = 0
	packet[6] = 0x02
	packet[7] = 0x8F
	packet[8] = 0
	packet[9] = 0
	packet[10] = 0x00
	packet[11] = 0x83
	copy(packet[12:16], []byte("GPS\x00"))
	writeNTPTimestamp(packet[32:40], receive)
	writeNTPTimestamp(packet[40:48], transmit)
	return packet
}

func TestParseNTPResponse(t *testing.T) {
	t1 := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	receive := t1.Add(4 * time.Millisecond)
	transmit := t1.Add(5 * time.Millisecond)
	t4 := t1.Add(10 * time.Millisecond)

	result, err := parseNTPResponse(makeNTPResponse(receive, transmit, 2, 0), t1, t4)
	if err != nil {
		t.Fatalf("parseNTPResponse failed: %v", err)
	}
	if math.Abs(result.OffsetMs-(-0.5)) > 0.01 {
		t.Fatalf("offset = %.3fms, want -0.5ms", result.OffsetMs)
	}
	if math.Abs(result.RTTMs-9) > 0.01 {
		t.Fatalf("rtt = %.3fms, want 9ms", result.RTTMs)
	}
	if result.Stratum != 2 {
		t.Fatalf("stratum = %d, want 2", result.Stratum)
	}
	if result.RefID != "GPS" {
		t.Fatalf("ref id = %q, want GPS", result.RefID)
	}
}

func TestParseNTPRejectsUnsynchronized(t *testing.T) {
	now := time.Now().UTC()
	if _, err := parseNTPResponse(makeNTPResponse(now, now, 16, 0), now, now); err == nil {
		t.Fatal("stratum 16 should be rejected")
	}
	if _, err := parseNTPResponse(makeNTPResponse(now, now, 2, 3), now, now); err == nil {
		t.Fatal("leap=3 should be rejected")
	}
}

func TestNTPMonitorHoldoverStatus(t *testing.T) {
	monitor := &NTPMonitor{
		enabled:       true,
		server:        "test.invalid",
		holdoverAfter: 20 * time.Minute,
		maxOffsetMs:   50,
		reachable:     true,
		lastSuccess:   time.Now().UTC().Add(-30 * time.Minute),
		lastAttempt:   time.Now().UTC().Add(-10 * time.Minute),
		stratum:       2,
	}
	status := monitor.Status()
	if !status.HasSample {
		t.Fatal("expected a sample")
	}
	if !status.Holdover {
		t.Fatal("expected holdover")
	}
	if status.Synchronized {
		t.Fatal("holdover must not be synchronized")
	}
	if status.AgeSeconds < 29*60 {
		t.Fatalf("age = %.1fs, want at least 29min", status.AgeSeconds)
	}
}

func TestNTPMonitorOffsetGate(t *testing.T) {
	monitor := &NTPMonitor{
		enabled:       true,
		server:        "test.invalid",
		holdoverAfter: 20 * time.Minute,
		maxOffsetMs:   50,
		reachable:     true,
		lastSuccess:   time.Now().UTC(),
		lastAttempt:   time.Now().UTC(),
		stratum:       2,
		offsetMs:      100,
	}
	status := monitor.Status()
	if status.OffsetOk {
		t.Fatal("offset above the gate must not be okay")
	}
	if status.Synchronized {
		t.Fatal("offset above the gate must not be synchronized")
	}
}

func TestNTPTimestampRoundTrip(t *testing.T) {
	input := time.Date(2026, 11, 2, 12, 0, 0, 123456789, time.UTC)
	raw := make([]byte, 8)
	writeNTPTimestamp(raw, input)
	output, ok := readNTPTimestamp(raw)
	if !ok {
		t.Fatal("expected timestamp")
	}
	if delta := output.Sub(input); delta > time.Nanosecond || delta < -time.Nanosecond {
		t.Fatalf("timestamp round trip delta = %v", delta)
	}
}

func TestNTPTimeHandlerShape(t *testing.T) {
	ntpMonitor = nil
	request := httptest.NewRequest("GET", "/v1/time/ntp", nil)
	recorder := httptest.NewRecorder()
	ntpTimeV1Handler(recorder, request)
	if recorder.Code != 200 {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	var response NTPTimeResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if response.ServerReceivedUnixUs <= 0 || response.ServerSentUnixUs <= 0 {
		t.Fatalf("missing server timestamps: %+v", response)
	}
	if response.ServerSentUnixUs < response.ServerReceivedUnixUs {
		t.Fatalf("server send before receive: %+v", response)
	}
	if response.ClockHealth.Error == "" {
		t.Fatal("expected an unavailable-monitor error when ntpMonitor is nil")
	}
	if response.ClockCorrectionApplied {
		t.Fatal("correction must not be applied without a monitor sample")
	}
	if response.ClockCorrectionMode != correctionModeFirst || response.ClockSource != "host" {
		t.Fatalf("unexpected clock fields: %+v", response)
	}
	if response.ClockCorrectionReason != "unavailable" {
		t.Fatalf("reason = %q, want unavailable", response.ClockCorrectionReason)
	}
	if response.ServerReceivedUnixUs != response.HostReceivedUnixUs {
		t.Fatal("server timestamp should equal host timestamp without correction")
	}
}

func TestCorrectionForHealth(t *testing.T) {
	health := NTPHealth{
		Enabled:       true,
		Server:        "first.example",
		Reachable:     true,
		HasSample:     true,
		Synchronized:  true,
		OffsetOk:      true,
		QualityOk:     true,
		OffsetMs:      8,
		UncertaintyMs: 15,
		AgeSeconds:    10,
	}
	decision := decideNTPServerCorrection(health, correctionModeFirst)
	if !decision.Applied || decision.Ms != 8 || decision.Reason != "primary_synchronized" {
		t.Fatalf("decision = %+v, want first-source correction", decision)
	}

	// The live 2026-11-06 case: fallback source, offset outside the trust gate,
	// quality acceptable, correction must stay off.
	fallback := NTPHealth{
		Enabled:       true,
		Server:        "fallback.example",
		Reachable:     true,
		HasSample:     true,
		Fallback:      true,
		OffsetOk:      false,
		QualityOk:     true,
		OffsetMs:      -15.26,
		UncertaintyMs: 15.60,
		AgeSeconds:    198,
	}
	decision = decideNTPServerCorrection(fallback, correctionModeFirst)
	if decision.Applied || decision.Eligible || decision.Reason != "fallback_source" {
		t.Fatalf("fallback decision = %+v, want no correction", decision)
	}

	health.Fallback = true
	if _, applied := correctionForHealth(health); applied {
		t.Fatal("fallback source must not apply correction")
	}
	health.Fallback = false
	health.OffsetOk = false
	if _, applied := correctionForHealth(health); applied {
		t.Fatal("offset gate failure must not apply correction")
	}
	health.OffsetOk = true
	health.UncertaintyMs = 30
	if _, applied := correctionForHealth(health); applied {
		t.Fatal("uncertainty above correction gate must not apply")
	}
	health.UncertaintyMs = 15
	health.Holdover = true
	if _, applied := correctionForHealth(health); applied {
		t.Fatal("holdover must not apply correction")
	}
	health.Holdover = false
	health.AgeSeconds = defaultCorrectionMaxAge.Seconds() + 1
	if _, applied := correctionForHealth(health); applied {
		t.Fatal("stale sample must not apply correction")
	}
	if decision := decideNTPServerCorrection(health, correctionModeOff); decision.Applied || decision.Reason != "mode_off" {
		t.Fatalf("off decision = %+v, want mode_off", decision)
	}
}

func TestNTPServerBetterResult(t *testing.T) {
	best := ntpQueryResult{UncertaintyMs: 20, RTTMs: 30}
	better := ntpQueryResult{UncertaintyMs: 10, RTTMs: 40}
	if !betterNTPResult(better, best) {
		t.Fatal("lower uncertainty must win")
	}
	sameUncertainty := ntpQueryResult{UncertaintyMs: 20, RTTMs: 10}
	if !betterNTPResult(sameUncertainty, best) {
		t.Fatal("lower RTT must break an uncertainty tie")
	}
	if betterNTPResult(ntpQueryResult{UncertaintyMs: 30, RTTMs: 5}, best) {
		t.Fatal("worse uncertainty must not win")
	}
}

func TestNTPEndpointsAcceptsLiteralIP(t *testing.T) {
	endpoints, err := ntpEndpoints("127.0.0.1")
	if err != nil {
		t.Fatalf("ntpEndpoints: %v", err)
	}
	if len(endpoints) != 1 || endpoints[0] != "127.0.0.1:123" {
		t.Fatalf("endpoints = %#v", endpoints)
	}
	endpoints, err = ntpEndpoints("::1")
	if err != nil {
		t.Fatalf("ntpEndpoints IPv6: %v", err)
	}
	if len(endpoints) != 1 || endpoints[0] != "[::1]:123" {
		t.Fatalf("IPv6 endpoints = %#v", endpoints)
	}
}

func TestEnvCorrectionMode(t *testing.T) {
	t.Setenv("NTP_CORRECTION_MODE", "off")
	if got := envCorrectionMode("NTP_CORRECTION_MODE", correctionModeFirst); got != correctionModeOff {
		t.Fatalf("mode = %q, want off", got)
	}
	t.Setenv("NTP_CORRECTION_MODE", "first")
	if got := envCorrectionMode("NTP_CORRECTION_MODE", correctionModeOff); got != correctionModeFirst {
		t.Fatalf("mode = %q, want first", got)
	}
	t.Setenv("NTP_CORRECTION_MODE", "bogus")
	if got := envCorrectionMode("NTP_CORRECTION_MODE", correctionModeFirst); got != correctionModeFirst {
		t.Fatalf("invalid mode = %q, want fallback first", got)
	}
}

func TestOpenAPISpecIncludesNTPMonitor(t *testing.T) {
	var document map[string]any
	if err := json.Unmarshal([]byte(openAPISpec), &document); err != nil {
		t.Fatalf("openAPISpec is not valid JSON: %v", err)
	}
	paths, ok := document["paths"].(map[string]any)
	if !ok {
		t.Fatal("openAPISpec has no paths")
	}
	if _, ok := paths["/time/ntp"]; !ok {
		t.Fatal("openAPISpec does not document /time/ntp")
	}
}

func TestNormalizeNTPProbeServerRejectsInvalidTargets(t *testing.T) {
	for _, value := range []string{
		"", "127.0.0.1", "::1", "localhost", "http://example.com",
		"example.com:123", "bad..example", "-bad.example",
		"bad-.example", "example", "foo.local", "foo.lan",
	} {
		if _, err := normalizeNTPProbeServer(value); err == nil {
			t.Fatalf("expected %q to be rejected", value)
		}
	}
}

func TestProbeNTPHealth(t *testing.T) {
	okHealth := probeNTPHealth("time.example", ntpQueryResult{
		OffsetMs:      2,
		RTTMs:         8,
		UncertaintyMs: 4,
		Stratum:       2,
		Leap:          0,
	}, nil, 10, 10)
	if !okHealth.Reachable || !okHealth.HasSample || !okHealth.OffsetOk || !okHealth.QualityOk || !okHealth.Synchronized {
		t.Fatalf("unexpected healthy probe: %+v", okHealth)
	}
	weakHealth := probeNTPHealth("time.example", ntpQueryResult{
		OffsetMs:      2,
		RTTMs:         121,
		UncertaintyMs: 86,
		Stratum:       3,
	}, nil, 10, 10)
	if weakHealth.Synchronized || weakHealth.QualityOk {
		t.Fatalf("expected quality gate to reject weak NTP: %+v", weakHealth)
	}
	failed := probeNTPHealth("time.example", ntpQueryResult{}, fmt.Errorf("timeout"), 10, 10)
	if failed.Reachable || failed.HasSample || failed.Synchronized || failed.Error == "" {
		t.Fatalf("unexpected failed probe: %+v", failed)
	}
}

func TestNTPProbeRejectsLiteralIP(t *testing.T) {
	request := httptest.NewRequest("GET", "/v1/time/ntp?server=127.0.0.1", nil)
	recorder := httptest.NewRecorder()
	ntpTimeV1Handler(recorder, request)
	if recorder.Code != 400 {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
}

func TestNTPHandlerRejectsServerAndRefreshTogether(t *testing.T) {
	request := httptest.NewRequest("GET", "/v1/time/ntp?server=time.cloudflare.com&refresh=1", nil)
	recorder := httptest.NewRecorder()
	ntpTimeV1Handler(recorder, request)
	if recorder.Code != 400 {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
}

func TestNTPRefreshRequiresMonitor(t *testing.T) {
	previous := ntpMonitor
	ntpMonitor = nil
	defer func() { ntpMonitor = previous }()
	request := httptest.NewRequest("GET", "/v1/time/ntp?refresh=1", nil)
	request.RemoteAddr = "198.51.100.7:1234"
	recorder := httptest.NewRecorder()
	ntpTimeV1Handler(recorder, request)
	if recorder.Code != 503 {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
}

func TestSelectNTPCandidateUsesPriorityAndQuality(t *testing.T) {
	candidates := []ntpCandidate{
		{server: "primary", index: 0, result: ntpQueryResult{OffsetMs: 2, UncertaintyMs: 80}},
		{server: "secondary", index: 1, result: ntpQueryResult{OffsetMs: 3, UncertaintyMs: 5}},
	}
	selected := selectNTPCandidate(candidates, 10, 10, 20)
	if selected == nil || selected.server != "secondary" {
		t.Fatalf("selected = %+v, want secondary fallback", selected)
	}

	weak := []ntpCandidate{
		{server: "primary", index: 0, result: ntpQueryResult{OffsetMs: 2, UncertaintyMs: 80}},
		{server: "secondary", index: 1, result: ntpQueryResult{OffsetMs: 3, UncertaintyMs: 90}},
	}
	selected = selectNTPCandidate(weak, 10, 10, 20)
	if selected == nil || selected.server != "primary" {
		t.Fatalf("selected = %+v, want highest-priority weak source", selected)
	}

	correctable := []ntpCandidate{
		{server: "primary", index: 0, result: ntpQueryResult{OffsetMs: 48, UncertaintyMs: 80}},
		{server: "secondary", index: 1, result: ntpQueryResult{OffsetMs: 48, UncertaintyMs: 15}},
	}
	selected = selectNTPCandidate(correctable, 10, 10, 20)
	if selected == nil || selected.server != "secondary" {
		t.Fatalf("selected = %+v, want correction-capable fallback", selected)
	}
	if selectNTPCandidate(nil, 10, 10, 20) != nil {
		t.Fatal("empty candidates should stay empty")
	}
}

func TestParseNTPServers(t *testing.T) {
	servers := parseNTPServers(" ntp.ntsc.ac.cn, time.cloudflare.com,ntp.ntsc.ac.cn,, ")
	if len(servers) != 2 || servers[0] != "ntp.ntsc.ac.cn" || servers[1] != "time.cloudflare.com" {
		t.Fatalf("servers = %#v", servers)
	}
	if got := parseNTPServers(""); len(got) != 1 || got[0] != defaultNTPServer {
		t.Fatalf("default servers = %#v", got)
	}
}

func TestNTPSourceStateTracksReachAndFailures(t *testing.T) {
	state := &ntpSourceState{}
	now := time.Now().UTC()
	state.record(true, now)
	state.record(true, now.Add(time.Minute))
	if state.reach != 0b11 || state.consecutiveFailures != 0 {
		t.Fatalf("after two successes: reach=%o failures=%d", state.reach, state.consecutiveFailures)
	}
	state.record(false, now.Add(2*time.Minute))
	state.record(false, now.Add(3*time.Minute))
	if state.reach != 0b1100 || state.consecutiveFailures != 2 {
		t.Fatalf("after two failures: reach=%o failures=%d", state.reach, state.consecutiveFailures)
	}
	state.record(true, now.Add(4*time.Minute))
	if state.reach != 0b11001 || state.consecutiveFailures != 0 {
		t.Fatalf("after recovery: reach=%o failures=%d", state.reach, state.consecutiveFailures)
	}
}
