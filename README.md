# Time API

A simple and efficient REST API for fetching current time information with timezone support.

## Features

- Get current time in various formats (timestamp, formatted time/date, ISO8601)
- Support for multiple timezones
- List all available timezones
- OpenAPI documentation
- CORS support for all endpoints (GET methods)
- Comprehensive logging with request IDs for traceability

## Installation

### Prerequisites

- Go 1.20 or higher
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

### Request IDs and Logging

All requests are assigned a unique request ID, which is included in the response headers as `X-Request-ID`. This ID is also included in all log messages related to the request, making it easy to trace the flow of a request through the system.

The API logs the following information in JSON format:
- Server startup and configuration
- Request details (method, path, remote address)
- Response details (status code, response time)
- Errors that occur during request processing

Each log entry includes:
- `timestamp`: ISO8601 timestamp with nanosecond precision
- `level`: Log level (info, error, fatal)
- `request_id`: The unique request ID (when applicable)
- `message`: The log message
- `data`: Additional structured data (when applicable)

Log format example (each line is a separate JSON object):
```
{"timestamp":"2023-06-24T12:34:56.789012345Z","level":"info","request_id":"1687612496789012345-123456","message":"Request: GET /v1/time 127.0.0.1:12345"}
{"timestamp":"2023-06-24T12:34:56.789123456Z","level":"info","request_id":"1687612496789012345-123456","message":"Processing time request with timezone: UTC"}
{"timestamp":"2023-06-24T12:34:56.789234567Z","level":"info","request_id":"1687612496789012345-123456","message":"Response: GET /v1/time - 200 OK - 1.234ms"}
```

The logs follow the [JSON Lines](https://jsonlines.org/) format, where each line is a valid JSON object.

The JSON format makes it easy to parse and analyze logs with tools like jq, Elasticsearch, or any log management system.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
