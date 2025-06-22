package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"time"

	timezone "github.com/tkuchiki/go-timezone"
)

// TimeResponse represents the response structure for the time endpoint
type TimeResponse struct {
	Timestamp int64  `json:"timestamp"`
	Time      string `json:"time"`
	Date      string `json:"date"`
	Timezone  string `json:"timezone"`
	Abbr      string `json:"abbr"`
	Offset    string `json:"offset"`
	ISO8601   string `json:"iso8601"`
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
          }
        },
        "required": ["timestamp", "time", "date", "timezone", "abbr", "offset", "iso8601"]
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

	// Get timezone from query parameters, default to UTC
	tzName := r.URL.Query().Get("timezone")

	if tzName == "" {
		tzName = "UTC"
	}

	tz := timezone.New()

	now, err := tz.FixedTimezone(time.Now(), tzName)
	if err != nil {
		// If timezone is invalid, return 400 Bad Request
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(ErrorResponse{Error: "Invalid timezone"})
		return
	}

	// Format the response
	response := TimeResponse{
		Timestamp: now.Unix(),
		Time:      now.Format("15:04:05.999999"),
		Date:      now.Format("2006-01-02"),
		Timezone:  tzName,
		Abbr:      tz.TzInfos()[tzName].ShortStandard(),
		Offset:    tz.TzInfos()[tzName].StandardOffsetHHMM(),
		ISO8601:   now.Format(time.RFC3339),
	}

	// Return the response
	json.NewEncoder(w).Encode(response)
}

func timezonesV1Handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	response := TimezonesResponse{
		Timezones: timezones,
	}

	json.NewEncoder(w).Encode(response)
}

func openAPIV1Handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, openAPISpec)
}

func main() {
	createTimeZoneList()

	// Register handlers
	http.HandleFunc("/v1/time", timeV1Handler)
	http.HandleFunc("/v1/timezones", timezonesV1Handler)
	http.HandleFunc("/v1/openapi.json", openAPIV1Handler)

	// Start the server
	port := 8080
	log.Printf("Server starting on port %d...", port)
	log.Fatal(http.ListenAndServe(":"+strconv.Itoa(port), nil))
}
