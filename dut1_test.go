package main

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const bulletinSample = `
      **********************************************************************
      *                   I E R S   B U L L E T I N - A                    *
      **********************************************************************
      COMBINED EARTH ORIENTATION PARAMETERS:

                              IERS Rapid Service
              MJD      x    error     y    error   UT1-UTC   error
                       "      "       "      "        s        s
   26 10  9  61322 0.16725 .00004 0.32272 .00004 -0.030145 0.000010
   26 10 10  61323 0.16629 .00004 0.32316 .00004 -0.031608 0.000011
   26 10 11  61324 0.16521 .00004 0.32353 .00004 -0.032999 0.000011
   26 10 15  61328 0.15856 .00004 0.32154 .00004 -0.036719 0.000016

      _______________________________________________________________________

      PREDICTIONS:
      The following formulas will not reproduce the predictions given below,
      but may be used to extend the predictions beyond the end of this table.

                    MJD      x(arcsec)   y(arcsec)   UT1-UTC(sec)
       2026 10 16  61329       0.1571      0.3210     -0.03722
       2026 10 17  61330       0.1556      0.3207     -0.03769
       2026 10 18  61331       0.1543      0.3205     -0.03825
       2026 10 19  61332       0.1531      0.3204     -0.03897
       2026 10 20  61333       0.1519      0.3204     -0.03989

      These predictions are based on all announced leap seconds.

      CELESTIAL POLE OFFSET SERIES:
                      61306  -123.36    0.12  -11.21    0.05
`

func TestParseDUT1BulletinA(t *testing.T) {
	points, err := parseDUT1Text(bulletinSample)
	if err != nil {
		t.Fatalf("parseDUT1Text returned error: %v", err)
	}
	if len(points) != 9 {
		t.Fatalf("got %d points, want 9: %#v", len(points), points)
	}

	observed := points["2026-10-15"]
	if observed.Type != "observed" {
		t.Errorf("2026-10-15 type = %q, want observed", observed.Type)
	}
	if observed.MJD != 61328 {
		t.Errorf("2026-10-15 MJD = %d, want 61328", observed.MJD)
	}
	if math.Abs(observed.Value-(-0.036719)) > 1e-12 {
		t.Errorf("2026-10-15 value = %v, want -0.036719", observed.Value)
	}

	predicted := points["2026-10-18"]
	if predicted.Type != "predicted" {
		t.Errorf("2026-10-18 type = %q, want predicted", predicted.Type)
	}
	if math.Abs(predicted.Value-(-0.03825)) > 1e-12 {
		t.Errorf("2026-10-18 value = %v, want -0.03825", predicted.Value)
	}

	// The celestial-pole rows must not be mistaken for DUT1 rows.
	if _, ok := points["61306"]; ok {
		t.Errorf("celestial pole row was parsed as a DUT1 point")
	}
}

func TestParseNTSCEOP(t *testing.T) {
	const ntsc = `# NTSC EOP example
2026 10 17 61330 0.1556 0.3207 -0.03769 0.00001 0.0000 0.0000 0.0000 0.0000 0.0000 0.0000 0.0000 0.0000
2026 10 18 61331 0.1543 0.3205 -0.03825 0.00001 0.0000 0.0000 0.0000 0.0000 0.0000 0.0000 0.0000 0.0000
`
	points, err := parseDUT1Text(ntsc)
	if err != nil {
		t.Fatalf("parseDUT1Text returned error: %v", err)
	}
	if len(points) != 2 {
		t.Fatalf("got %d points, want 2", len(points))
	}
	p := points["2026-10-18"]
	if p.Type != "observed" {
		t.Errorf("type = %q, want observed", p.Type)
	}
	if math.Abs(p.Value-(-0.03825)) > 1e-12 {
		t.Errorf("value = %v, want -0.03825", p.Value)
	}
}

func TestParseNTSCEOPPredictionCutoff(t *testing.T) {
	const ntsc = `#    Last date with real data:  2026-11-02
#    Last date with prediction: 2027-02-01
2026 11 2 61346 0.136114 0.317750 -0.0556760 0.0010051 0.000345 0.000000 0.999 0.999 0.999 0.999 0.999 0.999
2026 11 3 61347 0.134247 0.317549 -0.0568139 0.0011433 0.000342 0.000000 0.999 0.999 0.999 0.999 0.999 0.999
`
	points, err := parseDUT1Text(ntsc)
	if err != nil {
		t.Fatalf("parseDUT1Text returned error: %v", err)
	}
	if got := points["2026-11-02"].Type; got != "observed" {
		t.Errorf("2026-11-02 type = %q, want observed", got)
	}
	if got := points["2026-11-03"].Type; got != "predicted" {
		t.Errorf("2026-11-03 type = %q, want predicted", got)
	}
}

func TestNTSCServiceRefresh(t *testing.T) {
	const ntsc = `#    Last date with real data:  2026-11-02
2026 11 2 61346 0.136114 0.317750 -0.0556760 0.0010051 0.000345 0.000000 0.999 0.999 0.999 0.999 0.999 0.999
2026 11 3 61347 0.134247 0.317549 -0.0568139 0.0011433 0.000342 0.000000 0.999 0.999 0.999 0.999 0.999 0.999
`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ftpFile/findDataList":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"code":0,"data":[{"id":42,"fileName":"NTSC_EOP_20261103","createTime":"2026-11-03 15:30:00"}]}`)
		case "/ftpFile/download":
			if got := r.URL.Query().Get("fileId"); got != "42" {
				t.Errorf("unexpected fileId %q", got)
			}
			_, _ = io.WriteString(w, ntsc)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	svc := newNTSCDUT1Service(NewJSONLogger(io.Discard))
	svc.ntscBaseURL = server.URL
	svc.client = server.Client()
	if err := svc.refreshOnce(context.Background()); err != nil {
		t.Fatalf("refreshOnce returned error: %v", err)
	}
	info := svc.Info(time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC))
	if !info.Available || info.SourceKey != "ntsc" || info.Source != "NTSC EOP" {
		t.Fatalf("unexpected NTSC info: %+v", info)
	}
	if info.Current == nil || info.Current.Type != "predicted" {
		t.Fatalf("unexpected current point: %+v", info.Current)
	}
}

func TestParseDUT1RejectsEmptyData(t *testing.T) {
	if _, err := parseDUT1Text("not an EOP file\n"); err == nil {
		t.Fatal("expected an error for unsupported content")
	}
}

func TestAlternateDUT1Snapshot(t *testing.T) {
	oldIERS, oldNTSC := dut1, dut1NTSC
	defer func() { dut1, dut1NTSC = oldIERS, oldNTSC }()
	now := time.Now().UTC()
	dut1 = &dut1Service{
		source:     "IERS Bulletin A",
		sourceKey:  "iers",
		updatedAt:  now,
		staleAfter: time.Hour,
		points: map[string]DUT1Point{
			"2026-11-03": {Date: "2026-11-03", Value: -0.0581, Type: "predicted"},
		},
	}
	dut1NTSC = &dut1Service{
		source:     "NTSC EOP",
		sourceKey:  "ntsc",
		updatedAt:  now,
		staleAfter: time.Hour,
		points: map[string]DUT1Point{
			"2026-11-03": {Date: "2026-11-03", Value: -0.0568, Type: "predicted"},
		},
	}
	at := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	snapshot := alternateDUT1Snapshot("ntsc", at)
	if snapshot == nil || snapshot.SourceKey != "iers" || snapshot.Current == nil {
		t.Fatalf("unexpected alternate snapshot: %+v", snapshot)
	}
	if math.Abs(snapshot.Current.Value-(-0.0581)) > 1e-12 {
		t.Fatalf("alternate current value = %v, want -0.0581", snapshot.Current.Value)
	}
}

func TestDUT1InfoWindow(t *testing.T) {
	points, err := parseDUT1Text(bulletinSample)
	if err != nil {
		t.Fatal(err)
	}
	svc := &dut1Service{
		points:     points,
		source:     "IERS Bulletin A",
		sourceURL:  "https://example.invalid/bulletinA.txt",
		updatedAt:  time.Now().UTC(),
		staleAfter: time.Hour,
		logger:     NewJSONLogger(io.Discard),
	}

	info := svc.Info(time.Date(2026, 10, 18, 12, 34, 56, 0, time.UTC))
	if !info.Available {
		t.Fatal("expected available DUT1 data")
	}
	if info.Stale {
		t.Error("fresh data was reported as stale")
	}
	if info.Previous == nil || info.Previous.Date != "2026-10-17" {
		t.Fatalf("unexpected previous point: %#v", info.Previous)
	}
	if info.Current == nil || info.Current.Date != "2026-10-18" {
		t.Fatalf("unexpected current point: %#v", info.Current)
	}
	if info.Next == nil || info.Next.Date != "2026-10-19" {
		t.Fatalf("unexpected next point: %#v", info.Next)
	}
	if math.Abs(info.Current.Value-(-0.03825)) > 1e-12 {
		t.Errorf("current value = %v, want -0.03825", info.Current.Value)
	}

	// At UTC midnight the "current" slot must move to the next date without
	// requiring a new upstream refresh.
	afterMidnight := svc.Info(time.Date(2026, 10, 19, 0, 0, 1, 0, time.UTC))
	if afterMidnight.Current == nil || afterMidnight.Current.Date != "2026-10-19" {
		t.Fatalf("current point after midnight = %#v", afterMidnight.Current)
	}
	if afterMidnight.Previous == nil || afterMidnight.Previous.Date != "2026-10-18" {
		t.Fatalf("previous point after midnight = %#v", afterMidnight.Previous)
	}
}

func TestDUT1InfoUnavailable(t *testing.T) {
	svc := &dut1Service{
		updatedAt:  time.Time{},
		staleAfter: time.Hour,
		logger:     NewJSONLogger(io.Discard),
	}
	info := svc.Info(time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC))
	if info.Available {
		t.Fatal("empty cache must not be reported as available")
	}
	if info.Error == "" {
		t.Fatal("expected an error message")
	}
	if !info.Stale {
		t.Error("empty cache must be reported as stale")
	}
}

func TestDUT1ServiceRefresh(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, bulletinSample)
	}))
	defer server.Close()

	svc := &dut1Service{
		urls:       []string{server.URL + "/bulletinA.txt"},
		refresh:    time.Hour,
		staleAfter: 2 * time.Hour,
		timeout:    5 * time.Second,
		client:     server.Client(),
		logger:     NewJSONLogger(io.Discard),
	}
	if err := svc.refreshOnce(context.Background()); err != nil {
		t.Fatalf("refreshOnce returned error: %v", err)
	}

	info := svc.Info(time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC))
	if info.Source != "IERS Bulletin A" {
		t.Errorf("source = %q, want IERS Bulletin A", info.Source)
	}
	if !info.Available {
		t.Fatal("refreshed service has no current DUT1 value")
	}
}

func TestRoundDUT1(t *testing.T) {
	cases := []struct {
		in   float64
		want float64
	}{
		{-0.03825, 0},
		{0.03825, 0},
		{-0.052, -0.1},
		{0.052, 0.1},
		{-0.049, 0},
	}
	for _, tc := range cases {
		if got := roundDUT1(tc.in); got != tc.want {
			t.Errorf("roundDUT1(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
