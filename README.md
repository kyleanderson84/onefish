# OneFish

OneFish is a Go-based tool for interacting with Redfish API endpoints, commonly used for managing openBMC (Baseboard Management Controller) systems.

## Features

- Authentication support (basic auth and session-based with X-Auth-Token)
- Auto-detects when BMC requires credentials and prompts for `-u`/`-p`
- HTTP/HTTPS protocol handling with auto-detection
- HTTP Keep-Alive connection pooling for fast repeated requests
- `$expand` optimization to reduce request count on collections by ~95%
- **API Crawler**: Explore the Redfish API tree with automatic depth-first crawling
- **Interactive Mode**: Menu-driven interface for exploring endpoints, executing actions, and viewing data
- **Action Execution**: Run Redfish actions with guided parameter prompts and confirmation
- **CLI Command Generation**: Generate copy-paste commands from interactive sessions for automation
- **JSON Data Display**: View endpoint data inline or in a pager
- **Rate Limiting**: Optional, disabled by default (OpenBMC doesn't enforce it)

## Build

```bash
go build -o onefish
```

Requires Go 1.24+. No external dependencies.

## Usage

```bash
./onefish -target <ip:port> [-u <username> -p <password>]
```

If the BMC requires authentication and no credentials are provided, OneFish will exit with:

```
Authentication required but no credentials provided (use -u and -p)
```

### Interactive Mode

```bash
./onefish -target localhost:8000 -u admin -p password -i
```

Navigate the full Redfish API tree with single-key menu inputs:

```
============================================================
Current: Root Service (#ServiceRoot.v1_16_1.ServiceRoot)
Path: /redfish/v1/
------------------------------------------------------------
  0) Back
  1) #EventService.SubmitTestEvent
  A) Event Subscriptions Collection
  V) View Raw JSON
  C) CLI Commands
  Q) Exit

  ServiceEnabled: true
  Subscriptions: /redfish/v1/EventService/Subscriptions

>
```

**Menu keys:**

| Key | Action |
|-----|--------|
| `0` | Go back to previous endpoint |
| `1`-`9` | Execute actions |
| `A`-`Z` | Navigate to child endpoints |
| `V` | View raw JSON in pager |
| `C` | View CLI commands for session history |
| `Q` | Exit |

### API Crawler

```bash
./onefish -target localhost:8000 -u admin -p password --crawl
```

Crawls all Redfish API endpoints and displays a hierarchical tree. Uses `$expand` on collections to minimize requests.

```bash
# JSON output for programmatic use
./onefish -target localhost:8000 -u admin -p password --crawl --output json
```

### Action Execution

Execute Redfish actions directly from the command line:

```bash
./onefish -target localhost:8000 -u admin -p password \
  --action /redfish/v1/Chassis/1U/Actions/Chassis.Reset \
  --data '{"ResetType": "GracefulShutdown"}'
```

### Rate Limiting

Disabled by default. OpenBMC does not enforce a requests-per-second limit. Enable if the BMC starts pushing back:

```bash
# 1 second delay between requests
./onefish -target localhost:8000 -u admin -p password --crawl --rate-limit 1s

# 100ms delay for faster local testing
./onefish -target localhost:8000 -u admin -p password --crawl --rate-limit 100ms
```

## Complete Options

```
  -action string
    	Execute action (path)
  -crawl
    	Crawl the Redfish API tree
  -data string
    	JSON payload for action
  -i
    	Run in interactive mode
  -output string
    	Output format: tree, json (default "tree")
  -p string
    	Password
  -rate-limit string
    	Rate limit delay between requests (e.g., 1s, 500ms). Use '0s' to disable. (default "0s")
  -target string
    	BMC IP address and port
  -u string
    	Username
```
