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

func rootHandler(w http.ResponseWriter, r *http.Request) {
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
  "iso8601": "2021-10-18T10:13:44+00:00"
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
	fmt.Fprint(w, html)
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
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprint(w, `User-agent: *
Allow: /
`)
}

func main() {
	createTimeZoneList()

	// Register handlers with CORS middleware
	http.HandleFunc("/", corsMiddleware(rootHandler))
	http.HandleFunc("/v1/time", corsMiddleware(timeV1Handler))
	http.HandleFunc("/v1/timezones", corsMiddleware(timezonesV1Handler))
	http.HandleFunc("/v1/openapi.json", corsMiddleware(openAPIV1Handler))
	http.HandleFunc("/robots.txt", robotsTxtHandler)

	// Start the server
	port := 8080
	log.Printf("Server starting on port %d...", port)
	log.Fatal(http.ListenAndServe(":"+strconv.Itoa(port), nil))
}
