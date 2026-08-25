# OneFish

OneFish is a Go-based tool for interacting with Redfish API endpoints, commonly used for managing BMC (Baseboard Management Controller) systems.

## Features

- Authentication support (basic auth and session-based)
- HTTP/HTTPS protocol handling with auto-detection
- Command-line interface for easy usage
- Redfish API interaction capabilities
- **API Crawler**: Explore the Redfish API tree structure with automatic depth-first crawling
- **Rate Limiting**: Respects OpenBMC 1 request/second default (configurable)

## Usage

Basic usage:

```bash
./onefish -target <ip:port> [-u <username> -p <password>]
```

### API Crawler

Explore the Redfish API tree structure:

```bash
./onefish -target localhost:8000 --crawl
```

This recursively crawls all Redfish API endpoints and displays them in a hierarchical tree view.

### Rate Limiting

By default, onefish respects the typical OpenBMC rate limit of 1 request per second. This can be configured:

```bash
# Use default 1 second delay
./onefish -target localhost:8000 --crawl --rate-limit 1s

# Faster for local testing (100ms delay)
./onefish -target localhost:8000 --crawl --rate-limit 100ms

# Disable rate limiting for maximum speed (not recommended for real BMCs)
./onefish -target localhost:8000 --crawl --rate-limit 0s
```

### Output Formats

```bash
# Tree view (default)
./onefish -target localhost:8000 --crawl

# JSON output for programmatic use
./onefish -target localhost:8000 --crawl --output json
```

### Complete Options

```
  -crawl
    	Crawl the Redfish API tree
  -output string
    	Output format: tree, json (default "tree")
  -p string
    	Password
  -rate-limit string
    	Rate limit delay between requests (e.g., 1s, 500ms). Use '0s' or 'no-rate-limit' to disable. (default "1s")
  -target string
    	BMC IP address and port
  -u string
    	Username
```
