# Time API

A simple and efficient REST API for fetching current time information with timezone support.

## Features

- Get current time in various formats (timestamp, formatted time/date, ISO8601)
- Support for multiple timezones
- List all available timezones
- OpenAPI documentation
- CORS support for all endpoints (GET methods)

## Installation

### Prerequisites

- Go 1.16 or higher
- Docker (optional, for containerized deployment)

### Local Setup

1. Clone the repository:
   ```bash
   git clone https://github.com/petstack/time-api.git
   cd time-api
   ```

2. Install dependencies:
   ```bash
   go mod download
   ```

3. Run the server:
   ```bash
   go run main.go
   ```

   The server will start on port 8080 by default.

### Docker Setup

1. Build the Docker image:
   ```bash
   docker build -t time-api .
   ```

2. Run the container:
   ```bash
   docker run -p 8080:8080 time-api
   ```

## API Usage

### Get Current Time

```
GET /v1/time?timezone={timezone}
```

Parameters:
- `timezone` (optional): Timezone to get the time for (e.g., 'UTC', 'Europe/London'). Defaults to 'UTC'.

Example Response:
```json
{
  "timestamp": 1634567890,
  "time": "10:13:44.323435",
  "date": "2021-10-18",
  "timezone": "UTC",
  "abbr": "GMT",
  "offset": "+00:00",
  "iso8601": "2021-10-18T10:13:44+00:00"
}
```

### List Available Timezones

```
GET /v1/timezones
```

Example Response:
```json
{
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
  ]
}
```

### OpenAPI Documentation

```
GET /v1/openapi.json
```

Returns the OpenAPI specification for the API.

### CORS Support

All API endpoints support Cross-Origin Resource Sharing (CORS) for GET requests. The following CORS headers are included in all GET responses:

```
Access-Control-Allow-Origin: *
Access-Control-Allow-Methods: GET
Access-Control-Allow-Headers: Content-Type
```

This allows the API to be accessed from any domain using GET requests.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
