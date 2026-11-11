package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	timezone "github.com/tkuchiki/go-timezone"
)

// JSONLogger is a wrapper around slog.Logger that provides compatibility with our existing code
type JSONLogger struct {
	logger *slog.Logger
}

// Logger is a global logger instance
var Logger *JSONLogger

// NewJSONLogger creates a new JSON logger
func NewJSONLogger(w io.Writer) *JSONLogger {
	// Create a JSON handler with time in RFC3339Nano format
	opts := &slog.HandlerOptions{
		Level: slog.LevelInfo,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			// Format timestamp as RFC3339Nano
			if a.Key == slog.TimeKey {
				return slog.Attr{
					Key:   a.Key,
					Value: slog.StringValue(a.Value.Time().Format(time.RFC3339Nano)),
				}
			}
			return a
		},
	}
	handler := slog.NewJSONHandler(w, opts)
	logger := slog.New(handler)

	return &JSONLogger{logger: logger}
}

// Info logs an informational message
func (l *JSONLogger) Info(requestID, message string, data ...interface{}) {
	attrs := []slog.Attr{}

	if requestID != "" {
		attrs = append(attrs, slog.String("request_id", requestID))
	}

	if len(data) == 1 {
		attrs = append(attrs, slog.Any("data", data[0]))
	} else if len(data) > 1 {
		attrs = append(attrs, slog.Any("data", data))
	}

	l.logger.LogAttrs(context.Background(), slog.LevelInfo, message, attrs...)
}

// Error logs an error message
func (l *JSONLogger) Error(requestID, message string, data ...interface{}) {
	attrs := []slog.Attr{}

	if requestID != "" {
		attrs = append(attrs, slog.String("request_id", requestID))
	}

	if len(data) == 1 {
		attrs = append(attrs, slog.Any("data", data[0]))
	} else if len(data) > 1 {
		attrs = append(attrs, slog.Any("data", data))
	}

	l.logger.LogAttrs(context.Background(), slog.LevelError, message, attrs...)
}

// Fatal logs a fatal error message and exits the program
func (l *JSONLogger) Fatal(v ...interface{}) {
	message := fmt.Sprint(v...)
	l.logger.LogAttrs(context.Background(), slog.LevelError, message, slog.String("level", "fatal"))
	os.Exit(1)
}

// Printf is provided for compatibility with the standard logger
func (l *JSONLogger) Printf(format string, v ...interface{}) {
	message := fmt.Sprintf(format, v...)

	// Extract request ID if present
	requestID := ""
	if len(message) > 0 && message[0] == '[' {
		endIdx := strings.Index(message, "]")
		if endIdx > 0 {
			requestID = message[1:endIdx]
			message = strings.TrimSpace(message[endIdx+1:])
		}
	}

	attrs := []slog.Attr{}
	if requestID != "" {
		attrs = append(attrs, slog.String("request_id", requestID))
	}

	l.logger.LogAttrs(context.Background(), slog.LevelInfo, message, attrs...)
}

// Println is provided for compatibility with the standard logger
func (l *JSONLogger) Println(v ...interface{}) {
	message := fmt.Sprint(v...)
	l.logger.Info(message)
}

// TimeResponse represents the response structure for the time endpoint
type TimeResponse struct {
	Timestamp              int64     `json:"timestamp"`
	Time                   string    `json:"time"`
	Date                   string    `json:"date"`
	Timezone               string    `json:"timezone"`
	Abbr                   string    `json:"abbr"`
	Offset                 string    `json:"offset"`
	ISO8601                string    `json:"iso8601"`
	ServerReceivedMs       int64     `json:"server_received_ms"`
	ServerSentMs           int64     `json:"server_sent_ms"`
	ClockSource            string    `json:"clock_source"`
	ClockCorrectionApplied bool      `json:"clock_correction_applied"`
	DUT1                   *DUT1Info `json:"dut1,omitempty"`
}

type Timezone struct {
	Name string `json:"name"`
	Abbr string `json:"abbr"`
}
type TimezonesResponse struct {
	Timezones []Timezone `json:"timezones"`
}

// ErrorResponse represents the error response structure
type ErrorResponse struct {
	Error string `json:"error"`
}

var timezones []Timezone

// dut1 is the process-wide DUT1 cache. It is initialised in main.
var dut1 *dut1Service
var dut1NTSC *dut1Service

func createTimeZoneList() {
	tz := timezone.New()
	tzMap := tz.Timezones()

	// Получаем все ключи
	keys := make([]string, 0, len(tzMap))
	for k := range tzMap {
		keys = append(keys, k)
	}

	// Сортируем ключи
	sort.Strings(keys)

	// Обходим map в отсортированном порядке
	for _, k := range keys {
		for _, v := range tzMap[k] {
			timezones = append(timezones, Timezone{Name: v, Abbr: k})
		}
	}
}

// OpenAPI specification as a string constant
const openAPISpec = `{
  "openapi": "3.0.3",
  "info": {
    "title": "Time API",
    "version": "1.0.0",
    "description": "API for fetching current time information"
  },
  "servers": [
    {
      "url": "https://gettimeapi.dev/v1",
      "description": "Production server"
    },
    {
      "url": "http://localhost:8080/v1",
      "description": "Local Dev server"
    }
  ],
  "paths": {
    "/time": {
      "get": {
        "summary": "Get current time",
        "parameters": [
          {
            "name": "timezone",
            "in": "query",
            "required": false,
            "schema": {
              "type": "string"
            },
            "description": "Timezone to get the time for, e.g., 'UTC' or 'Europe/London'"
          }
        ],
        "responses": {
          "200": {
            "description": "Current time details",
            "content": {
              "application/json": {
                "schema": {
                  "$ref": "#/components/schemas/TimeResponse"
                }
              }
            }
          },
          "400": {
            "description": "Invalid timezone provided",
            "content": {
              "application/json": {
                "schema": {
                  "$ref": "#/components/schemas/ErrorResponse"
                }
              }
            }
          }
        }
      }
    },
    "/time/ntp": {
      "get": {
        "summary": "Get compact NTP-oriented time and backend clock health",
        "description": "Microsecond server timestamps plus upstream NTP monitor status for browser four-timestamp synchronization. Pass server=<domain> for a one-off probe of another public NTP server; this does not change the main monitor. Pass refresh=1 to query the configured monitor source immediately.",
        "parameters": [
          {
            "name": "server",
            "in": "query",
            "required": false,
            "schema": {"type": "string"},
            "description": "Optional public DNS name for a one-off NTP probe. IP literals and private/local names are rejected. Rate limited."
          },
          {
            "name": "refresh",
            "in": "query",
            "required": false,
            "schema": {"type": "boolean"},
            "description": "Refresh the configured monitor once before returning. Do not combine with server. Rate limited."
          }
        ],
        "responses": {
          "200": {
            "description": "Compact time transfer response",
            "content": {
              "application/json": {
                "schema": {
                  "$ref": "#/components/schemas/NTPTimeResponse"
                }
              }
            }
          },
          "400": {
            "description": "Invalid NTP probe target or conflicting parameters"
          },
          "429": {
            "description": "NTP probe rate limit"
          },
          "503": {
            "description": "NTP monitor disabled"
          }
        }
      }
    },
    "/dut1": {
      "get": {
        "summary": "Get DUT1 (UT1-UTC) for a UTC date and its neighbours",
        "description": "Returns the published UT1-UTC values for the requested UTC date, the day before and the day after. Values that have not yet been observed are marked as predicted. The source can be auto (prefer fresh IERS, then NTSC fallback), ntsc, or iers.",
        "parameters": [
          {
            "name": "date",
            "in": "query",
            "required": false,
            "schema": {
              "type": "string",
              "format": "date"
            },
            "description": "UTC date in YYYY-MM-DD format. Defaults to the current UTC date."
          },
          {
            "name": "source",
            "in": "query",
            "required": false,
            "schema": {
              "type": "string",
              "enum": ["auto", "ntsc", "iers"]
            },
            "description": "DUT1 source selection. Defaults to DUT1_DEFAULT_SOURCE or auto."
          }
        ],
        "responses": {
          "200": {
            "description": "DUT1 values for the requested date and its neighbours",
            "content": {
              "application/json": {
                "schema": {
                  "$ref": "#/components/schemas/DUT1Info"
                }
              }
            }
          },
          "400": {
            "description": "Invalid date provided",
            "content": {
              "application/json": {
                "schema": {
                  "$ref": "#/components/schemas/ErrorResponse"
                }
              }
            }
          }
        }
      }
    },
    "/timezones": {
      "get": {
        "summary": "List available timezones",
        "responses": {
          "200": {
            "description": "List of available timezones",
            "content": {
              "application/json": {
                "schema": {
                  "type": "object",
                  "properties": {
                    "timezones": {
                      "type": "array",
                      "items": {
                        "$ref": "#/components/schemas/Timezone"
                      }
                    }
                  },
                  "required": ["timezones"]
                }
              }
            }
          }
        }
      }
    }
  },
  "components": {
    "schemas": {
      "TimeResponse": {
        "type": "object",
        "properties": {
          "timestamp": {
            "type": "integer",
            "description": "Current Unix timestamp"
          },
          "time": {
            "type": "string",
            "example": "10:13:44.3234353",
            "description": "Current time in HH:MM:SS.ffffff format"
          },
          "date": {
            "type": "string",
            "example": "2025-07-22",
            "description": "Current date in YYYY-MM-DD format"
          },
          "timezone": {
            "type": "string",
            "example": "UTC",
            "description": "Timezone name"
          },
          "abbr": {
            "type": "string",
            "example": "GMT",
            "description": "Timezone abbreviation"
          },
          "offset": {
            "type": "string",
            "example": "+00:00",
            "description": "Timezone offset in HH:MM format"
          },
          "iso8601": {
            "type": "string",
            "example": "2004-02-12T15:19:21+00:00",
            "description": "Date and time in ISO 8601 format"
          },
          "server_received_ms": {
            "type": "integer",
            "description": "Server receive time as Unix milliseconds; used for four-timestamp clock offset estimation"
          },
          "server_sent_ms": {
            "type": "integer",
            "description": "Server send time as Unix milliseconds; used for four-timestamp clock offset estimation"
          },
          "clock_source": {
            "type": "string",
            "enum": ["host"],
            "description": "This endpoint always serves the raw host clock"
          },
          "clock_correction_applied": {
            "type": "boolean",
            "description": "Always false for /v1/time; use /v1/time/ntp for monitored correction metadata"
          },
          "dut1": {
            "$ref": "#/components/schemas/DUT1Info"
          }
        },
        "required": ["timestamp", "time", "date", "timezone", "abbr", "offset", "iso8601"]
      },
      "NTPTimeResponse": {
        "type": "object",
        "properties": {
          "sequence": {
            "type": "integer",
            "format": "int64",
            "description": "Monotonic per-process response counter"
          },
          "server_received_unix_us": {
            "type": "integer",
            "format": "int64",
            "description": "Server receive time as Unix microseconds"
          },
          "server_sent_unix_us": {
            "type": "integer",
            "format": "int64",
            "description": "Server send time as Unix microseconds"
          },
          "server_received_ms": {
            "type": "integer",
            "format": "int64",
            "description": "Server receive time as Unix milliseconds"
          },
          "server_sent_ms": {
            "type": "integer",
            "format": "int64",
            "description": "Server send time as Unix milliseconds"
          },
          "host_received_unix_us": {
            "type": "integer",
            "format": "int64",
            "description": "Raw host receive time before NTP correction"
          },
          "host_sent_unix_us": {
            "type": "integer",
            "format": "int64",
            "description": "Raw host send time before NTP correction"
          },
          "host_received_ms": {
            "type": "integer",
            "format": "int64"
          },
          "host_sent_ms": {
            "type": "integer",
            "format": "int64"
          },
          "clock_correction_ms": {
            "type": "number",
            "description": "NTP-host bias applied to server timestamps; zero when not applied"
          },
          "clock_correction_applied": {
            "type": "boolean"
          },
          "clock_correction_eligible": {
            "type": "boolean",
            "description": "True when the configured correction policy allows this source"
          },
          "clock_correction_mode": {
            "type": "string",
            "enum": ["first", "off"],
            "description": "configured correction policy; first means only the first configured source"
          },
          "clock_correction_source": {
            "type": "string",
            "description": "source that supplied the correction decision"
          },
          "clock_correction_reason": {
            "type": "string",
            "description": "machine-readable reason such as primary_synchronized, fallback_source, offset_gate, probe_only"
          },
          "clock_source": {
            "type": "string",
            "enum": ["host", "host+first-source"],
            "description": "Which clock basis produced server_received/server_sent"
          },
          "clock_correction_uncertainty_ms": {
            "type": "number"
          },
          "date": {
            "type": "string",
            "example": "2026-11-02"
          },
          "time": {
            "type": "string",
            "example": "12:30:14.701234"
          },
          "probe": {
            "type": "boolean",
            "description": "True when this response came from a one-off ?server= probe"
          },
          "refreshed": {
            "type": "boolean",
            "description": "True when this response came from an explicit ?refresh=1 monitor refresh"
          },
          "clock_health": {
            "$ref": "#/components/schemas/NTPHealth"
          }
        },
        "required": ["sequence", "server_received_unix_us", "server_sent_unix_us", "date", "time", "clock_health"]
      },
      "NTPSourceHealth": {
        "type": "object",
        "properties": {
          "server": { "type": "string" },
          "resolved_ip": { "type": "string", "description": "IP address actually probed for this DNS name" },
          "reachable": { "type": "boolean" },
          "selected": { "type": "boolean" },
          "offset_ms": { "type": "number" },
          "uncertainty_ms": { "type": "number" },
          "rtt_ms": { "type": "number" },
          "stratum": { "type": "integer" },
          "offset_ok": { "type": "boolean" },
          "quality_ok": { "type": "boolean" },
          "root_delay_ms": { "type": "number" },
          "root_dispersion_ms": { "type": "number" },
          "leap": { "type": "integer" },
          "last_success_at": { "type": "string" },
          "last_attempt_at": { "type": "string" },
          "age_s": { "type": "number" },
          "consecutive_failures": { "type": "integer" },
          "reach": { "type": "integer" },
          "reach_octal": { "type": "string" },
          "error": { "type": "string" }
        }
      },
      "NTPHealth": {
        "type": "object",
        "properties": {
          "enabled": { "type": "boolean" },
          "server": { "type": "string" },
          "servers": {
            "type": "array",
            "items": {"type": "string"},
            "description": "Configured priority list"
          },
          "sources": {
            "type": "array",
            "items": { "$ref": "#/components/schemas/NTPSourceHealth" },
            "description": "Per-source observations from the latest poll"
          },
          "fallback": {
            "type": "boolean",
            "description": "True when the selected source is not the first configured source"
          },
          "reachable": { "type": "boolean" },
          "synchronized": { "type": "boolean" },
          "holdover": { "type": "boolean" },
          "has_sample": { "type": "boolean" },
          "offset_ok": { "type": "boolean" },
          "quality_ok": { "type": "boolean" },
          "offset_ms": { "type": "number" },
          "uncertainty_ms": { "type": "number" },
          "rtt_ms": { "type": "number" },
          "stratum": { "type": "integer" },
          "leap": { "type": "integer" },
          "ref_id": { "type": "string" },
          "root_delay_ms": { "type": "number" },
          "root_dispersion_ms": { "type": "number" },
          "last_success_at": { "type": "string" },
          "last_attempt_at": { "type": "string" },
          "resolved_ip": { "type": "string", "description": "IP address used for the selected source" },
          "age_s": { "type": "number" },
          "error": { "type": "string" }
        },
        "required": ["enabled", "server", "reachable", "synchronized", "holdover", "has_sample"]
      },
      "DUT1Point": {
        "type": "object",
        "properties": {
          "date": {
            "type": "string",
            "example": "2026-10-18",
            "description": "UTC date in YYYY-MM-DD format"
          },
          "mjd": {
            "type": "integer",
            "example": 61331,
            "description": "Modified Julian Date"
          },
          "value": {
            "type": "number",
            "example": -0.03825,
            "description": "UT1-UTC in seconds"
          },
          "dut1": {
            "type": "number",
            "example": 0.0,
            "description": "UT1-UTC rounded to the 0.1 s DUT1 step used by time signals"
          },
          "type": {
            "type": "string",
            "enum": ["observed", "predicted"],
            "description": "Whether the value is an observed measurement or a prediction"
          }
        },
        "required": ["date", "value", "dut1", "type"]
      },
      "DUT1Info": {
        "type": "object",
        "properties": {
          "date": {
            "type": "string",
            "example": "2026-10-18",
            "description": "Requested UTC date"
          },
          "date_basis": {
            "type": "string",
            "example": "UTC",
            "description": "Calendar basis of the date fields"
          },
          "unit": {
            "type": "string",
            "example": "s",
            "description": "Unit of the value fields (seconds)"
          },
          "source": {
            "type": "string",
            "example": "IERS Bulletin A",
            "description": "Name of the upstream DUT1 source"
          },
          "source_key": {
            "type": "string",
            "enum": ["ntsc", "iers"],
            "description": "Stable identifier of the upstream DUT1 source"
          },
          "source_mode": {
            "type": "string",
            "enum": ["auto", "ntsc", "iers"],
            "description": "Requested source selection mode"
          },
          "source_url": {
            "type": "string",
            "description": "URL of the upstream data file"
          },
          "updated_at": {
            "type": "string",
            "format": "date-time",
            "description": "Time of the most recent successful refresh"
          },
          "stale": {
            "type": "boolean",
            "description": "True when the cached data is older than the configured threshold"
          },
          "available": {
            "type": "boolean",
            "description": "True when a value for the requested date is available"
          },
          "previous": {
            "$ref": "#/components/schemas/DUT1Point"
          },
          "current": {
            "$ref": "#/components/schemas/DUT1Point"
          },
          "next": {
            "$ref": "#/components/schemas/DUT1Point"
          },
          "alternate": {
            "$ref": "#/components/schemas/DUT1SourceSnapshot"
          },
          "error": {
            "type": "string",
            "description": "Human-readable reason when DUT1 data is unavailable"
          }
        },
        "required": ["date", "date_basis", "unit", "source", "stale", "available"]
      },
      "DUT1SourceSnapshot": {
        "type": "object",
        "properties": {
          "source": {
            "type": "string",
            "description": "Alternate upstream source name"
          },
          "source_key": {
            "type": "string",
            "enum": ["ntsc", "iers"]
          },
          "updated_at": {
            "type": "string"
          },
          "stale": {
            "type": "boolean"
          },
          "available": {
            "type": "boolean"
          },
          "previous": {
            "$ref": "#/components/schemas/DUT1Point"
          },
          "current": {
            "$ref": "#/components/schemas/DUT1Point"
          },
          "next": {
            "$ref": "#/components/schemas/DUT1Point"
          },
          "error": {
            "type": "string"
          }
        }
      },
      "ErrorResponse": {
        "type": "object",
        "properties": {
          "error": {
            "type": "string",
            "example": "Invalid timezone",
            "description": "Description of the error"
          }
        },
        "required": ["error"]
      },
      "Timezone": {
        "type": "object",
        "properties": {
          "name": {"type": "string", "description": "Timezone name"},
          "abbr": {"type": "string", "description": "Timezone abbreviation"}
        },
        "required": ["name", "abbr"]
      }
    }
  }
}`

func timeV1Handler(w http.ResponseWriter, r *http.Request) {
	// Set content type
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store, max-age=0")
	received := time.Now().UTC()

	// Get timezone from query parameters, default to UTC
	tzName := r.URL.Query().Get("timezone")

	if tzName == "" {
		tzName = "UTC"
	}

	// Get request ID for logging
	requestID := w.Header().Get("X-Request-ID")

	Logger.Printf("[%s] Processing time request with timezone: %s", requestID, tzName)

	tz := timezone.New()

	now, err := tz.FixedTimezone(time.Now(), tzName)
	if err != nil {
		// If timezone is invalid, return 400 Bad Request
		Logger.Printf("[%s] Error: Invalid timezone '%s': %v", requestID, tzName, err)
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(ErrorResponse{Error: "Invalid timezone"})
		return
	}

	// Format the response
	var dut1Info *DUT1Info
	if dut1 != nil || dut1NTSC != nil {
		info := dut1InfoFor(now, "")
		dut1Info = &info
	}

	sent := time.Now().UTC()
	response := TimeResponse{
		Timestamp:              now.Unix(),
		Time:                   now.Format("15:04:05.999999"),
		Date:                   now.Format("2006-01-02"),
		Timezone:               tzName,
		Abbr:                   tz.TzInfos()[tzName].ShortStandard(),
		Offset:                 tz.TzInfos()[tzName].StandardOffsetHHMM(),
		ISO8601:                now.Format(time.RFC3339),
		ServerReceivedMs:       received.UnixMilli(),
		ServerSentMs:           sent.UnixMilli(),
		ClockSource:            "host",
		ClockCorrectionApplied: false,
		DUT1:                   dut1Info,
	}

	// Return the response
	if err := json.NewEncoder(w).Encode(response); err != nil {
		Logger.Printf("[%s] Error encoding response: %v", requestID, err)
	}
}

func timezonesV1Handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Get request ID for logging
	requestID := w.Header().Get("X-Request-ID")
	Logger.Printf("[%s] Processing timezones request", requestID)

	response := TimezonesResponse{
		Timezones: timezones,
	}

	if err := json.NewEncoder(w).Encode(response); err != nil {
		Logger.Printf("[%s] Error encoding timezones response: %v", requestID, err)
	}
}

func openAPIV1Handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Get request ID for logging
	requestID := w.Header().Get("X-Request-ID")
	Logger.Printf("[%s] Processing OpenAPI spec request", requestID)

	_, err := fmt.Fprint(w, openAPISpec)
	if err != nil {
		Logger.Printf("[%s] Error writing OpenAPI spec: %v", requestID, err)
	}
}

func rootHandler(w http.ResponseWriter, r *http.Request) {
	// Get request ID for logging
	requestID := w.Header().Get("X-Request-ID")
	Logger.Printf("[%s] Processing root page request", requestID)

	html := `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Time API Documentation</title>
    <style>
        body {
            font-family: Arial, sans-serif;
            line-height: 1.6;
            max-width: 800px;
            margin: 0 auto;
            padding: 20px;
        }
        h1, h2 {
            color: #333;
        }
        code {
            background-color: #f4f4f4;
            padding: 2px 5px;
            border-radius: 3px;
            font-family: monospace;
        }
        pre {
            background-color: #f4f4f4;
            padding: 10px;
            border-radius: 5px;
            overflow-x: auto;
        }
        .endpoint {
            margin-bottom: 30px;
        }
    </style>
</head>
<body>
    <h1>Time API</h1>
    <p>A simple and efficient REST API for fetching current time information with timezone support.</p>
    <p>GitHub Repository: <a href="https://github.com/petstack/time-api" target="_blank">https://github.com/petstack/time-api</a></p>

    <h2>API Endpoints</h2>

    <div class="endpoint">
        <h3>Get Current Time</h3>
        <code>GET /v1/time?timezone={timezone}</code>
        <p>Parameters:</p>
        <ul>
            <li><code>timezone</code> (optional): Timezone to get the time for (e.g., 'UTC', 'Europe/London'). Defaults to 'UTC'.</li>
        </ul>
        <p>Example Response:</p>
        <pre>{
  "timestamp": 1634567890,
  "time": "10:13:44.323435",
  "date": "2021-10-18",
  "timezone": "UTC",
  "abbr": "GMT",
  "offset": "+00:00",
  "iso8601": "2021-10-18T10:13:44+00:00",
  "dut1": {
    "date": "2021-10-18",
    "date_basis": "UTC",
    "unit": "s",
    "source": "IERS Bulletin A",
    "stale": false,
    "available": true,
    "previous": { "date": "2021-10-17", "value": 0.0312, "dut1": 0, "type": "observed" },
    "current":  { "date": "2021-10-18", "value": 0.0298, "dut1": 0, "type": "observed" },
    "next":     { "date": "2021-10-19", "value": 0.0284, "dut1": 0, "type": "predicted" }
  }
}</pre>
    </div>

    <div class="endpoint">
        <h3>Get DUT1 (UT1-UTC) for a Date and its Neighbours</h3>
        <code>GET /v1/dut1?date={yyyy-MM-dd}&amp;source={auto|ntsc|iers}</code>
        <p>Parameters:</p>
        <ul>
            <li><code>date</code> (optional): UTC calendar date in YYYY-MM-DD format. Defaults to the current UTC date.</li>
            <li><code>source</code> (optional): <code>auto</code> (prefer fresh IERS, then NTSC fallback), <code>ntsc</code>, or <code>iers</code>.</li>
        </ul>
        <p>Returns the published UT1-UTC value for the requested date together with the day before and the day after. Values that have not yet been observed are marked as <code>predicted</code>. The value is in seconds; <code>dut1</code> is the same value rounded to the 0.1 s step used by time-signal broadcasts.</p>
        <p>Example Response:</p>
        <pre>{
  "date": "2026-10-18",
  "date_basis": "UTC",
  "unit": "s",
  "source": "IERS Bulletin A",
  "source_key": "iers",
  "source_mode": "auto",
  "source_url": "https://datacenter.iers.org/data/latestVersion/bulletinA.txt",
  "updated_at": "2026-10-18T08:00:00Z",
  "stale": false,
  "available": true,
  "previous": { "date": "2026-10-17", "mjd": 61330, "value": -0.03769, "dut1": 0, "type": "predicted" },
  "current":  { "date": "2026-10-18", "mjd": 61331, "value": -0.03825, "dut1": 0, "type": "predicted" },
  "next":     { "date": "2026-10-19", "mjd": 61332, "value": -0.03897, "dut1": 0, "type": "predicted" }
}</pre>
    </div>

    <div class="endpoint">
        <h3>List Available Timezones</h3>
        <code>GET /v1/timezones</code>
        <p>Example Response:</p>
        <pre>{
  "timezones": [
    {
      "name": "Africa/Abidjan",
      "abbr": "GMT"
    },
    {
      "name": "Africa/Accra",
      "abbr": "GMT"
    },
    {
      "name": "Europe/London",
      "abbr": "BST"
    }
    // ... more timezones
  ]
}</pre>
    </div>

    <div class="endpoint">
        <h3>OpenAPI Documentation</h3>
        <code>GET /v1/openapi.json</code>
        <p>Returns the OpenAPI specification for the API.</p>
    </div>

    <h2>CORS Support</h2>
    <p>All API endpoints support Cross-Origin Resource Sharing (CORS) for GET requests. The following CORS headers are included in all GET responses:</p>
    <pre>Access-Control-Allow-Origin: *
Access-Control-Allow-Methods: GET
Access-Control-Allow-Headers: Content-Type</pre>
    <p>This allows the API to be accessed from any domain using GET requests.</p>
</body>
</html>`

	w.Header().Set("Content-Type", "text/html")
	_, err := fmt.Fprint(w, html)
	if err != nil {
		Logger.Printf("[%s] Error writing HTML response: %v", requestID, err)
	}
}

// corsMiddleware adds CORS headers to GET requests
func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Add CORS headers for GET requests
		if r.Method == http.MethodGet {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		}
		next(w, r)
	}
}

func robotsTxtHandler(w http.ResponseWriter, r *http.Request) {
	// Get request ID for logging
	requestID := w.Header().Get("X-Request-ID")
	Logger.Printf("[%s] Processing robots.txt request", requestID)

	w.Header().Set("Content-Type", "text/plain")
	_, err := fmt.Fprint(w, `User-agent: *
Allow: /
`)
	if err != nil {
		Logger.Printf("[%s] Error writing robots.txt response: %v", requestID, err)
	}
}

// notFoundHandler handles 404 errors and returns different formats based on Accept header
func notFoundHandler(w http.ResponseWriter, r *http.Request) {
	// Get request ID for logging
	requestID := w.Header().Get("X-Request-ID")
	Logger.Printf("[%s] 404 Not Found: %s %s", requestID, r.Method, r.URL.Path)

	// Check Accept header to determine response format
	accept := r.Header.Get("Accept")
	Logger.Printf("[%s] Accept header: %s", requestID, accept)

	// If Accept header contains text/html, return HTML response
	if accept != "" && (strings.Contains(accept, "text/html") || accept == "*/*" || strings.Contains(accept, "text/*")) {
		w.Header().Set("Content-Type", "text/html")
		// Set status code after all headers are set
		w.WriteHeader(http.StatusNotFound)
		html := `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>404 - Page Not Found</title>
    <style>
        body {
            font-family: Arial, sans-serif;
            line-height: 1.6;
            max-width: 800px;
            margin: 0 auto;
            padding: 20px;
            text-align: center;
        }
        h1 {
            color: #333;
            font-size: 36px;
            margin-bottom: 20px;
        }
        p {
            font-size: 18px;
            margin-bottom: 30px;
        }
        a {
            color: #0066cc;
            text-decoration: none;
        }
        a:hover {
            text-decoration: underline;
        }
    </style>
</head>
<body>
    <h1>404 - Page Not Found</h1>
    <p>The page you are looking for does not exist.</p>
    <p><a href="/">Return to Home</a></p>
</body>
</html>`
		_, err := fmt.Fprint(w, html)
		if err != nil {
			Logger.Printf("[%s] Error writing HTML 404 response: %v", requestID, err)
		}
		Logger.Printf("[%s] Returned HTML 404 response", requestID)
		return
	}

	// Default to JSON response
	w.Header().Set("Content-Type", "application/json")
	// Set status code after all headers are set
	w.WriteHeader(http.StatusNotFound)
	err := json.NewEncoder(w).Encode(ErrorResponse{Error: "Page not found"})
	if err != nil {
		Logger.Printf("[%s] Error encoding JSON 404 response: %v", requestID, err)
	}
	Logger.Printf("[%s] Returned JSON 404 response", requestID)
}

// getClientIP gets the client IP from headers or remote address
func getClientIP(r *http.Request) string {
	// First check DO-Connecting-IP header
	if ip := r.Header.Get("DO-Connecting-IP"); ip != "" {
		return ip
	}

	// Then check X-Forwarded-For header
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		// X-Forwarded-For can contain multiple IPs, use the first one
		ips := strings.Split(ip, ",")
		if len(ips) > 0 {
			return strings.TrimSpace(ips[0])
		}
	}

	// Fall back to RemoteAddr
	return r.RemoteAddr
}

// loggingMiddleware logs HTTP requests with details
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Create a response wrapper to capture the status code
		rw := &responseWriter{
			ResponseWriter: w,
			statusCode:     http.StatusOK, // Default to 200 OK
		}

		// Get the request ID from the header (if already set)
		requestID := w.Header().Get("X-Request-ID")
		if requestID == "" {
			requestID = "unknown"
		}

		// Get client IP using the helper function
		clientIP := getClientIP(r)

		// Log the request
		Logger.Printf("[%s] Request: %s %s - IP: %s - User-Agent: %s", requestID, r.Method, r.URL.Path, clientIP, r.Header.Get("User-Agent"))

		// Call the next handler
		next.ServeHTTP(rw, r)

		// Log the response
		duration := time.Since(start)
		Logger.Printf("[%s] Response: %s %s - %d %s - %v - IP: %s - User-Agent: %s",
			requestID,
			r.Method,
			r.URL.Path,
			rw.statusCode,
			http.StatusText(rw.statusCode),
			duration,
			clientIP,
			r.Header.Get("User-Agent"))
	})
}

// responseWriter is a wrapper for http.ResponseWriter that captures the status code
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

// WriteHeader captures the status code before writing it
func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

func main() {
	// Initialize the logger
	Logger = NewJSONLogger(os.Stdout)
	Logger.Println("Initializing Time API server...")

	createTimeZoneList()
	Logger.Printf("Loaded %d timezones", len(timezones))

	// Start the DUT1 cache. The first refresh is attempted synchronously so
	// that early requests can already return the current UT1-UTC value. If
	// the upstream source is unreachable, the server still starts and the
	// refresh loop retries in the background.
	dut1 = newDUT1Service(Logger)
	dut1.Start(context.Background())
	if envBool("DUT1_NTSC_ENABLED", true) {
		dut1NTSC = newNTSCDUT1Service(Logger)
		// Restore the fallback cache without contacting NTSC. The network path is
		// started below only when IERS is unavailable, or on demand later.
		dut1NTSC.LoadCache()
		now := time.Now()
		if !dut1.hasFresh(now) && !dut1NTSC.hasFresh(now) {
			dut1NTSC.EnsureStarted()
		}
	}

	// Observe the upstream NTP source without touching the host clock. This
	// gives the browser a way to distinguish "host follows UTC" from "host is in
	// holdover after losing its NTP source".
	ntpMonitor = newNTPMonitorFromEnv()
	ntpMonitor.Start(context.Background())

	// Create a custom mux with our notFoundHandler
	mux := http.NewServeMux()

	// Register handlers with CORS middleware
	mux.HandleFunc("/", rootHandler)
	mux.HandleFunc("/v1/time", corsMiddleware(timeV1Handler))
	mux.HandleFunc("/v1/time/ntp", corsMiddleware(ntpTimeV1Handler))
	mux.HandleFunc("/v1/dut1", corsMiddleware(dut1V1Handler))
	mux.HandleFunc("/v1/timezones", corsMiddleware(timezonesV1Handler))
	mux.HandleFunc("/v1/openapi.json", corsMiddleware(openAPIV1Handler))
	mux.HandleFunc("/robots.txt", robotsTxtHandler)

	// Create a custom server with our mux
	port := 8080
	Logger.Printf("Configuring server on port %d", port)

	server := &http.Server{
		Addr: fmt.Sprintf(":%d", port),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Generate a unique request ID
			requestID := fmt.Sprintf("%d-%d", time.Now().UnixNano(), rand.Intn(1000000))
			// Add X-Request-ID header
			w.Header().Set("X-Request-ID", requestID)

			// Check if the path exists in our mux
			h, pattern := mux.Handler(r)
			if pattern != r.URL.Path {
				notFoundHandler(w, r)
				return
			}

			// Apply logging middleware
			loggingMiddleware(h).ServeHTTP(w, r)
		}),
	}

	// Start the server
	Logger.Printf("Server starting on port %d...", port)
	Logger.Fatal(server.ListenAndServe())
}
