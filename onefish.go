package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

type Client struct {
	target   string
	username string
	password string
	token    string
	useHTTP  bool
	insecure bool
	http     *http.Client
}

type RateLimiter struct {
	minDelay time.Duration
	lastReq  time.Time
}

func NewRateLimiter(delay string) (*RateLimiter, error) {
	d, err := time.ParseDuration(delay)
	if err != nil {
		return nil, err
	}
	return &RateLimiter{
		minDelay: d,
		lastReq:  time.Time{},
	}, nil
}

func (rl *RateLimiter) Wait() {
	if rl.lastReq.IsZero() {
		rl.lastReq = time.Now()
		return
	}
	elapsed := time.Since(rl.lastReq)
	if elapsed < rl.minDelay {
		time.Sleep(rl.minDelay - elapsed)
	}
	rl.lastReq = time.Now()
}

func NewClient(target, username, password string, insecure bool) *Client {
	transport := &http.Transport{
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     30 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: insecure},
	}
	return &Client{
		target:   target,
		username: username,
		password: password,
		insecure: insecure,
		http:     &http.Client{Transport: transport},
	}
}

func (c *Client) Authenticate() error {
	if c.username == "" && c.password == "" {
		resp, err := c.doRequest("GET", "/redfish/v1/", nil, nil)
		if err != nil {
			return fmt.Errorf("connection failed: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return fmt.Errorf("authentication required but no credentials provided (use -u and -p)")
		}
		return nil
	}

	if err := c.tryBasicAuth(); err == nil {
		return nil
	}

	return c.createSession()
}

func (c *Client) buildAuthHeaders() map[string]string {
	headers := map[string]string{}
	if c.username != "" && c.password != "" {
		if c.token != "" {
			headers["X-Auth-Token"] = c.token
		} else {
			encoded := base64.StdEncoding.EncodeToString([]byte(c.username + ":" + c.password))
			headers["Authorization"] = "Basic " + encoded
		}
	}
	return headers
}

func (c *Client) doRequest(method, url string, body io.Reader, headers map[string]string) (*http.Response, error) {
	fullURL := fmt.Sprintf("https://%s%s", c.target, url)

	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, err
	}

	if headers == nil {
		headers = c.buildAuthHeaders()
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)

	if err != nil && strings.Contains(err.Error(), "tls: first record does not look like a TLS handshake") {
		c.useHTTP = true
		fullURL = fmt.Sprintf("http://%s%s", c.target, url)

		req, err = http.NewRequest(method, fullURL, body)
		if err != nil {
			return nil, err
		}

		for k, v := range headers {
			req.Header.Set(k, v)
		}

		resp, err = c.http.Do(req)
	}

	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (c *Client) tryBasicAuth() error {
	resp, err := c.doRequest("GET", "/redfish/v1/", nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("basic auth failed with status: %d", resp.StatusCode)
	}

	return nil
}

func (c *Client) createSession() error {
	sessionData := map[string]string{
		"UserName": c.username,
		"Password": c.password,
	}

	jsonData, err := json.Marshal(sessionData)
	if err != nil {
		return err
	}

	resp, err := c.doRequest("POST", "/redfish/v1/SessionService/Sessions", bytes.NewBuffer(jsonData), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("session creation failed with status: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var sessionResponse struct {
		Token string `json:"Token"`
	}

	if err := json.Unmarshal(body, &sessionResponse); err != nil {
		return err
	}

	c.token = sessionResponse.Token
	return nil
}

func (c *Client) Get(url string) (*http.Response, error) {
	return c.doRequest("GET", url, nil, nil)
}

func (c *Client) GetWithRateLimit(url string, rl *RateLimiter) (*http.Response, error) {
	if rl != nil {
		rl.Wait()
	}
	return c.Get(url)
}

type ResourceNode struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	Name     string                 `json:"name"`
	Children []ResourceNode         `json:"children,omitempty"`
	Actions  map[string]string      `json:"actions,omitempty"`
	Links    map[string]string      `json:"links,omitempty"`
	Raw      map[string]interface{} `json:"raw,omitempty"`
}

type Crawler struct {
	client      *Client
	rateLimiter *RateLimiter
	visited     map[string]bool
}

func NewCrawler(client *Client, rl *RateLimiter) *Crawler {
	return &Crawler{
		client:      client,
		rateLimiter: rl,
		visited:     make(map[string]bool),
	}
}

func (c *Crawler) Crawl(url string) (*ResourceNode, error) {
	normalizedURL := url
	if len(normalizedURL) > 1 && normalizedURL[len(normalizedURL)-1] == '/' {
		normalizedURL = normalizedURL[:len(normalizedURL)-1]
	}

	if c.visited[normalizedURL] {
		return nil, nil
	}
	c.visited[normalizedURL] = true

	resp, err := c.client.GetWithRateLimit(url, c.rateLimiter)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var data map[string]interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("failed to parse JSON: %w", err)
	}

	node := &ResourceNode{
		ID:  url,
		Raw: data,
	}

	if v, ok := data["@odata.id"].(string); ok {
		node.ID = v
	}
	if v, ok := data["@odata.type"].(string); ok {
		node.Type = v
	}
	if v, ok := data["Name"].(string); ok {
		node.Name = v
	}

	if err := c.processMembers(data, node); err != nil {
		return nil, err
	}

	if err := c.processActions(data, node); err != nil {
		return nil, err
	}

	if err := c.processLinks(data, node); err != nil {
		return nil, err
	}

	if err := c.processCollections(data, node, url); err != nil {
		return nil, err
	}

	if err := c.processSubCollections(data, node, url); err != nil {
		return nil, err
	}

	return node, nil
}

func (c *Crawler) processMembers(data map[string]interface{}, node *ResourceNode) error {
	if members, ok := data["Members"].([]interface{}); ok {
		for _, member := range members {
			if memberMap, ok := member.(map[string]interface{}); ok {
				if id, ok := memberMap["@odata.id"].(string); ok {
					child, err := c.Crawl(id)
					if err != nil {
						return err
					}
					if child != nil {
						node.Children = append(node.Children, *child)
					}
				}
			}
		}
	}
	return nil
}

func (c *Crawler) processActions(data map[string]interface{}, node *ResourceNode) error {
	if actions, ok := data["Actions"].(map[string]interface{}); ok {
		node.Actions = make(map[string]string)
		for actionKey, actionVal := range actions {
			if actionMap, ok := actionVal.(map[string]interface{}); ok {
				if target, ok := actionMap["target"].(string); ok {
					node.Actions[actionKey] = target
				}
			}
		}
	}
	return nil
}

func (c *Crawler) processLinks(data map[string]interface{}, node *ResourceNode) error {
	if links, ok := data["Links"].(map[string]interface{}); ok {
		node.Links = make(map[string]string)
		c.extractLinks(links, "", node.Links)
	}
	return nil
}

func (c *Crawler) extractLinks(links map[string]interface{}, prefix string, result map[string]string) {
	for key, val := range links {
		fullKey := key
		if prefix != "" {
			fullKey = prefix + "." + key
		}
		if linkMap, ok := val.(map[string]interface{}); ok {
			if id, ok := linkMap["@odata.id"].(string); ok {
				result[fullKey] = id
			} else if members, ok := linkMap["Members"].([]interface{}); ok {
				for i, member := range members {
					if memberMap, ok := member.(map[string]interface{}); ok {
						if id, ok := memberMap["@odata.id"].(string); ok {
							result[fmt.Sprintf("%s[%d]", fullKey, i)] = id
						}
					}
				}
			}
		} else if id, ok := val.(string); ok {
			result[fullKey] = id
		}
	}
}

func (c *Crawler) processCollections(data map[string]interface{}, node *ResourceNode, baseURL string) error {
	collectionFields := []string{"Systems", "Chassis", "Managers", "AccountService", "SessionService",
		"TaskService", "UpdateService", "EventService", "CertificateService", "KeyService", "Registries"}

	for _, field := range collectionFields {
		if link, ok := data[field].(map[string]interface{}); ok {
			if id, ok := link["@odata.id"].(string); ok {
				fullID := id
				if !strings.HasPrefix(id, "http") {
					fullID = baseURL
					if !strings.HasSuffix(baseURL, "/") && !strings.HasPrefix(id, "/") {
						fullID += "/"
					}
					fullID += id
				}

				expandURL := fullID + "?$expand=.($levels=1)"
				resp, err := c.client.GetWithRateLimit(expandURL, c.rateLimiter)
				if err != nil {
					resp, err = c.client.GetWithRateLimit(fullID, c.rateLimiter)
					if err != nil {
						continue
					}
				}

				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					continue
				}

				var collectionData map[string]interface{}
				if err := json.Unmarshal(body, &collectionData); err != nil {
					continue
				}

				if members, ok := collectionData["Members"].([]interface{}); ok {
					for _, member := range members {
						if memberMap, ok := member.(map[string]interface{}); ok {
							if memberID, ok := memberMap["@odata.id"].(string); ok {
								child, err := c.Crawl(memberID)
								if err != nil {
									continue
								}
								if child != nil {
									node.Children = append(node.Children, *child)
								}
							}
						}
					}
				}
			}
		}
	}

	return nil
}

func (c *Crawler) processSubCollections(data map[string]interface{}, node *ResourceNode, baseURL string) error {
	subCollectionFields := []string{"EthernetInterfaces", "SerialInterfaces", "LogServices", "Memory", "Processors",
		"SimpleStorage", "Storage", "SecureBoot", "Bios", "VirtualMedia", "USBControllers", "GraphicsControllers",
		"Certificates", "Power", "Thermal", "PowerSubsystem", "ThermalSubsystem", "Sensors",
		"Controls", "EnvironmentMetrics", "HostInterfaces", "NetworkProtocol", "SecurityPolicy",
		"DedicatedNetworkPorts", "SerialConsole", "GraphicalConsole", "CommandShell", "HostEthernetInterfaces"}

	for _, field := range subCollectionFields {
		if link, ok := data[field].(map[string]interface{}); ok {
			if id, ok := link["@odata.id"].(string); ok {
				fullID := id
				if !strings.HasPrefix(id, "http") && !strings.HasPrefix(id, "/redfish") {
					fullID = baseURL
					if !strings.HasSuffix(baseURL, "/") && !strings.HasPrefix(id, "/") {
						fullID += "/"
					}
					fullID += id
				}

				expandURL := fullID + "?$expand=.($levels=1)"
				resp, err := c.client.GetWithRateLimit(expandURL, c.rateLimiter)
				if err != nil {
					resp, err = c.client.GetWithRateLimit(fullID, c.rateLimiter)
					if err != nil {
						continue
					}
				}

				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					continue
				}

				var subData map[string]interface{}
				if err := json.Unmarshal(body, &subData); err != nil {
					continue
				}

				if members, ok := subData["Members"].([]interface{}); ok {
					for _, member := range members {
						if memberMap, ok := member.(map[string]interface{}); ok {
							if memberID, ok := memberMap["@odata.id"].(string); ok {
								child, err := c.Crawl(memberID)
								if err != nil {
									continue
								}
								if child != nil {
									node.Children = append(node.Children, *child)
								}
							}
						}
					}
				}
			}
		}
	}

	return nil
}

func printTree(node *ResourceNode, prefix string, isLast bool) {
	connector := "└── "
	if !isLast {
		connector = "├── "
	}

	name := node.Name
	if name == "" {
		name = node.ID
	}
	fmt.Printf("%s%s%s\n", prefix, connector, name)

	children := node.Children
	links := node.Links

	if len(children) == 0 && len(links) == 0 {
		return
	}

	newPrefix := prefix
	if isLast {
		newPrefix += "    "
	} else {
		newPrefix += "│   "
	}

	allKeys := make([]string, 0, len(links))
	for k := range links {
		allKeys = append(allKeys, k)
	}
	sort.Strings(allKeys)

	for _, key := range allKeys {
		fmt.Printf("%s%s%s: %s\n", newPrefix, connector, key, links[key])
	}

	childPrefix := newPrefix
	if len(allKeys) > 0 {
		childPrefix += "    "
	}

	for i, child := range children {
		printTree(&child, childPrefix, i == len(children)-1)
	}
}

// ============================================================================
// Color Constants
// ============================================================================

const (
	ColorReset  = "\033[0m"
	ColorBlue   = "\033[34m"
	ColorGreen  = "\033[32m"
	ColorYellow = "\033[33m"
	ColorRed    = "\033[31m"
	ColorCyan   = "\033[36m"
	ColorPurple = "\033[35m"
)

// ============================================================================
// Interactive Mode Data Types
// ============================================================================

type InteractiveSession struct {
	client      *Client
	rateLimiter *RateLimiter
	history     []string
	visited     map[string]bool
	cached      map[string]*ResourceNode
	hideCreds   bool
}

type MenuOption struct {
	ID         int
	Label      string
	Path       string
	OptType    string
	ActionData map[string]string
	ParentPath string
	Key        string
}

type ActionParam struct {
	Name     string
	Type     string
	Required bool
	Values   []string
	Input    string
}

func NewInteractiveSession(client *Client, rl *RateLimiter, hideCreds bool) *InteractiveSession {
	return &InteractiveSession{
		client:      client,
		rateLimiter: rl,
		history:     make([]string, 0),
		visited:     make(map[string]bool),
		cached:      make(map[string]*ResourceNode),
		hideCreds:   hideCreds,
	}
}

// ============================================================================
// Interactive Mode Helper Functions
// ============================================================================

const inputReader = "onefish_input"

var reader = bufio.NewReader(os.Stdin)

func readLine() string {
	line, _ := reader.ReadString('\n')
	return strings.TrimRight(line, "\r\n")
}

func clearScreen() {
	fmt.Print("\033[2J\033[H")
}

func promptEnter() {
	fmt.Printf("\n%sPress Enter to continue...%s", ColorYellow, ColorReset)
	reader.ReadString('\n')
}

func truncatePath(path string, maxLen int) string {
	if len(path) <= maxLen {
		return path
	}
	lastSlash := strings.LastIndex(path, "/")
	if lastSlash < 0 {
		return path
	}
	lastSegment := path[lastSlash+1:]
	remaining := maxLen - len(lastSegment) - 4
	if remaining <= 0 {
		return ".../" + lastSegment
	}
	start := lastSlash
	count := remaining
	for count > 0 && start > 0 {
		start--
		if path[start] == '/' {
			count--
		}
	}
	if start <= 0 {
		return "..." + path[lastSlash:]
	}
	return path[:start+1] + "..." + path[lastSlash:]
}

func extractIDFromPath(path string) string {
	parts := strings.Split(path, "/")
	return parts[len(parts)-1]
}

// ============================================================================
// Interactive Mode - Endpoint Fetching
// ============================================================================

func (s *InteractiveSession) fetchEndpoint(path string) (*ResourceNode, error) {
	if node, ok := s.cached[path]; ok {
		return node, nil
	}

	if s.rateLimiter != nil {
		s.rateLimiter.Wait()
	}
	resp, err := s.client.doRequest("GET", path, nil, nil)
	if err != nil {
		if strings.Contains(err.Error(), "tls: first record does not look like a TLS handshake") {
			s.client.useHTTP = true
			resp, err = s.client.doRequest("GET", path, nil, nil)
		}
		if err != nil {
			return nil, fmt.Errorf("failed to fetch %s: %w", path, err)
		}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read: %w", err)
	}

	var data map[string]interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("failed to parse JSON: %w", err)
	}

	node := &ResourceNode{ID: path, Raw: data}
	if v, ok := data["@odata.id"].(string); ok {
		node.ID = v
	}
	if v, ok := data["@odata.type"].(string); ok {
		node.Type = v
	}
	if v, ok := data["Name"].(string); ok {
		node.Name = v
	}

	if actions, ok := data["Actions"].(map[string]interface{}); ok {
		node.Actions = make(map[string]string)
		for key, val := range actions {
			if actionMap, ok := val.(map[string]interface{}); ok {
				if target, ok := actionMap["target"].(string); ok {
					node.Actions[key] = target
				}
			}
		}
	}

	if links, ok := data["Links"].(map[string]interface{}); ok {
		node.Links = make(map[string]string)
		s.extractLinks(links, "", node.Links)
	}

	if members, ok := data["Members"].([]interface{}); ok {
		for _, member := range members {
			if memberMap, ok := member.(map[string]interface{}); ok {
				if id, ok := memberMap["@odata.id"].(string); ok {
					child, err := s.fetchEndpoint(id)
					if err == nil && child != nil {
						node.Children = append(node.Children, *child)
					}
				}
			}
		}
	}

	skipFields := map[string]bool{
		"@odata.id": true, "@odata.type": true, "@odata.context": true,
		"Name": true, "Description": true, "Id": true,
		"Status": true, "PowerState": true, "Actions": true,
		"Links": true, "Members": true, "Members@odata.count": true,
	}
	for key, val := range data {
		if skipFields[key] {
			continue
		}
		if objMap, ok := val.(map[string]interface{}); ok {
			if id, ok := objMap["@odata.id"].(string); ok {
				dup := false
				for _, c := range node.Children {
					if c.ID == id {
						dup = true
						break
					}
				}
				if !dup {
					child, err := s.fetchEndpoint(id)
					if err == nil && child != nil {
						if child.Name == "" {
							child.Name = key
						}
						node.Children = append(node.Children, *child)
					}
				}
			}
		}
	}

	s.cached[path] = node
	return node, nil
}

func (s *InteractiveSession) extractLinks(links map[string]interface{}, prefix string, result map[string]string) {
	for key, val := range links {
		fullKey := key
		if prefix != "" {
			fullKey = prefix + "." + key
		}
		if linkMap, ok := val.(map[string]interface{}); ok {
			if id, ok := linkMap["@odata.id"].(string); ok {
				result[fullKey] = id
			}
			if members, ok := linkMap["Members"].([]interface{}); ok {
				for i, member := range members {
					if memberMap, ok := member.(map[string]interface{}); ok {
						if id, ok := memberMap["@odata.id"].(string); ok {
							result[fmt.Sprintf("%s[%d]", fullKey, i)] = id
						}
					}
				}
			}
		} else if id, ok := val.(string); ok {
			result[fullKey] = id
		}
	}
}

// ============================================================================
// Interactive Mode - Display Functions
// ============================================================================

func displayEndpointInfo(node *ResourceNode) {
	fmt.Printf("\n%s%s%s\n", ColorCyan, strings.Repeat("=", 60), ColorReset)
	name := node.Name
	if name == "" {
		name = extractIDFromPath(node.ID)
	}
	fmt.Printf("%sCurrent:%s %s", ColorCyan, ColorReset, name)
	if node.Type != "" {
		fmt.Printf(" (%s)", node.Type)
	}
	fmt.Println()
	if status, ok := node.Raw["Status"].(map[string]interface{}); ok {
		state, health := "Unknown", "Unknown"
		if v, _ := status["State"].(string); v != "" {
			state = v
		}
		if v, _ := status["Health"].(string); v != "" {
			health = v
		}
		healthColor := ColorGreen
		if health == "Warning" {
			healthColor = ColorYellow
		}
		if health == "Critical" || state == "Error" {
			healthColor = ColorRed
		}
		fmt.Printf("%sStatus:%s %s%s%s | %s%s%s\n", ColorCyan, ColorReset, healthColor, health, ColorReset, healthColor, state, ColorReset)
	}
	if power, ok := node.Raw["PowerState"].(string); ok {
		fmt.Printf("%sPower:%s %s\n", ColorCyan, ColorReset, power)
	}
	fmt.Printf("%sPath:%s %s\n", ColorCyan, ColorReset, truncatePath(node.ID, 58))
	fmt.Printf("%s%s%s\n", ColorCyan, strings.Repeat("-", 60), ColorReset)
}

func displayEndpointData(node *ResourceNode) {
	skipFields := map[string]bool{
		"@odata.id": true, "@odata.type": true, "@odata.context": true,
		"@odata.etag": true, "@odata.navigationLink": true,
		"Name": true, "Description": true, "Id": true,
		"Status": true, "Actions": true, "Links": true,
		"Members": true, "Members@odata.count": true,
	}

	keys := make([]string, 0, len(node.Raw))
	for k := range node.Raw {
		if !skipFields[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	for _, key := range keys {
		val := node.Raw[key]
		displayValue(key, val, "")
	}
}

func displayValue(key string, val interface{}, prefix string) {
	fullKey := prefix + key
	switch v := val.(type) {
	case map[string]interface{}:
		_, hasID := v["@odata.id"]
		if hasID {
			fmt.Printf("  %s%s%s: %s%s%s\n", ColorBlue, fullKey, ColorReset, ColorGreen, v["@odata.id"], ColorReset)
		} else {
			flat := true
			for _, sv := range v {
				if _, isObj := sv.(map[string]interface{}); isObj {
					flat = false
					break
				}
				if _, isArr := sv.([]interface{}); isArr {
					flat = false
					break
				}
			}
			if flat {
				parts := make([]string, 0, len(v))
				for _, k2 := range sortedKeys(v) {
					parts = append(parts, fmt.Sprintf("%s=%v", k2, v[k2]))
				}
				fmt.Printf("  %s%s%s: %s\n", ColorBlue, fullKey, ColorReset, strings.Join(parts, ", "))
			} else {
				for _, k2 := range sortedKeys(v) {
					displayValue(k2, v[k2], fullKey+".")
				}
			}
		}
	case []interface{}:
		if len(v) > 0 {
			first := v[0]
			if _, isObj := first.(map[string]interface{}); isObj {
				fmt.Printf("  %s%s%s: (%d items)\n", ColorBlue, fullKey, ColorReset, len(v))
			} else {
				strs := make([]string, 0, len(v))
				for _, item := range v {
					strs = append(strs, fmt.Sprintf("%v", item))
				}
				fmt.Printf("  %s%s%s: %s\n", ColorBlue, fullKey, ColorReset, strings.Join(strs, ", "))
			}
		}
	case nil:
		// skip
	case bool:
		fmt.Printf("  %s%s%s: %v\n", ColorBlue, fullKey, ColorReset, v)
	case float64:
		if v == float64(int64(v)) {
			fmt.Printf("  %s%s%s: %d\n", ColorBlue, fullKey, ColorReset, int64(v))
		} else {
			fmt.Printf("  %s%s%s: %v\n", ColorBlue, fullKey, ColorReset, v)
		}
	default:
		s := fmt.Sprintf("%v", v)
		if s != "" && s != "null" {
			fmt.Printf("  %s%s%s: %s\n", ColorBlue, fullKey, ColorReset, s)
		}
	}
}

func sortedKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ============================================================================
// Interactive Mode - Menu Building
// ============================================================================

func buildMenu(session *InteractiveSession, node *ResourceNode) []MenuOption {
	options := make([]MenuOption, 0)
	currentPath := session.history[len(session.history)-1]

	if len(session.history) > 1 {
		options = append(options, MenuOption{
			ID:         1,
			Label:      "0) Back",
			Path:       session.history[len(session.history)-2],
			OptType:    "back",
			ParentPath: currentPath,
			Key:        "0",
		})
	}

	actionIdx := 0
	actionNames := make([]string, 0, len(node.Actions))
	for name := range node.Actions {
		actionNames = append(actionNames, name)
	}
	sort.Strings(actionNames)
	for _, name := range actionNames {
		if actionIdx >= 9 {
			break
		}
		target := node.Actions[name]
		key := fmt.Sprintf("%d", actionIdx+1)
		options = append(options, MenuOption{
			ID:         len(options) + 1,
			Label:      fmt.Sprintf("%s) %s", key, truncatePath(name, 50)),
			Path:       target,
			OptType:    "action",
			ActionData: map[string]string{"name": name, "target": target},
			ParentPath: currentPath,
			Key:        key,
		})
		actionIdx++
	}

	reserved := map[string]bool{"V": true, "C": true, "Q": true, "0": true}
	for i := 1; i <= 9; i++ {
		reserved[fmt.Sprintf("%d", i)] = true
	}
	letterIdx := 0
	allLetters := "ABCDEFGHIJKLMNOPQRSTUVWXYZ"

	navigable := make([]MenuOption, 0)
	for _, child := range node.Children {
		label := child.Name
		if label == "" {
			label = extractIDFromPath(child.ID)
		}
		navigable = append(navigable, MenuOption{
			Label:      truncatePath(label, 50),
			Path:       child.ID,
			OptType:    "member",
			ParentPath: currentPath,
		})
	}
	linkNames := make([]string, 0, len(node.Links))
	for name := range node.Links {
		linkNames = append(linkNames, name)
	}
	sort.Strings(linkNames)
	for _, name := range linkNames {
		navigable = append(navigable, MenuOption{
			Label:      truncatePath(name, 50),
			Path:       node.Links[name],
			OptType:    "link",
			ParentPath: currentPath,
		})
	}

	for _, nav := range navigable {
		for letterIdx < len(allLetters) {
			key := string(allLetters[letterIdx])
			letterIdx++
			if !reserved[key] {
				nav.ID = len(options) + 1
				nav.Label = fmt.Sprintf("%s) %s", key, nav.Label)
				nav.Key = key
				options = append(options, nav)
				break
			}
		}
	}

	options = append(options,
		MenuOption{ID: len(options) + 1, Label: "V) View Raw JSON", OptType: "json", ParentPath: currentPath, Key: "V"},
		MenuOption{ID: len(options) + 2, Label: "C) CLI Commands", OptType: "cli", ParentPath: currentPath, Key: "C"},
		MenuOption{ID: len(options) + 3, Label: "Q) Exit", OptType: "exit", Key: "Q"},
	)

	return options
}

func displayMenu(options []MenuOption) {
	for _, opt := range options {
		fmt.Printf("  %s\n", opt.Label)
	}
	fmt.Printf("\n%s> %s", ColorYellow, ColorReset)
}

// ============================================================================
// Interactive Mode - Input Handling
// ============================================================================

func readChoice(options []MenuOption) (MenuOption, bool) {
	input := readLine()
	input = strings.TrimSpace(input)
	inputUpper := strings.ToUpper(input)

	for _, opt := range options {
		if strings.ToUpper(opt.Key) == inputUpper && opt.Key != "" {
			return opt, true
		}
	}

	return MenuOption{}, false
}

// ============================================================================
// Interactive Mode - Action Handling
// ============================================================================

func handleAction(session *InteractiveSession, actionTarget string, actionData map[string]string) {
	fmt.Printf("\n%sAction:%s %s\n", ColorGreen, ColorReset, actionData["name"])
	fmt.Printf("%sTarget:%s %s\n\n", ColorGreen, ColorReset, actionTarget)

	currentPath := session.history[len(session.history)-1]
	currentEndpoint := session.cached[currentPath]

	params := promptActionParams(actionData["name"], currentEndpoint.Raw)
	payload := buildActionPayload(params)

	fmt.Printf("\n%sOptions:%s\n", ColorYellow, ColorReset)
	fmt.Printf("  [1] Execute Action\n")
	fmt.Printf("  [2] Show CLI Command\n")
	fmt.Printf("  [3] Cancel\n")
	fmt.Printf("\n%s> %s", ColorYellow, ColorReset)

	sel := readLine()
	switch sel {
	case "1":
		executeAction(session, actionTarget, payload)
	case "2":
		cli := generateCLICommand(session, "", actionTarget, payload)
		fmt.Printf("\n%s%s%s\n", ColorCyan, cli, ColorReset)
		promptEnter()
	case "3":
		return
	}
}

func promptActionParams(actionName string, rawData map[string]interface{}) []ActionParam {
	params := make([]ActionParam, 0)

	// Look for AllowableValues in the raw data to discover parameters
	for key, val := range rawData {
		if strings.HasSuffix(key, "AllowableValues") {
			paramName := strings.TrimSuffix(key, "AllowableValues")
			if arr, ok := val.([]interface{}); ok {
				values := make([]string, len(arr))
				for i, v := range arr {
					values[i] = fmt.Sprintf("%v", v)
				}
				params = append(params, ActionParam{
					Name:     paramName,
					Type:     "enum",
					Required: true,
					Values:   values,
				})
			}
		}
	}

	// Also look for action input parameters in the Redfish Actions structure
	// by checking for non-AllowableValues, non-target keys in action definitions
	if strings.Contains(actionName, "#") {
		parts := strings.SplitN(actionName, "#", 2)
		if len(parts) == 2 {
			actionDef := parts[1]
			// The parameter names are typically after the last dot
			_ = actionDef
		}
	}

	for i := range params {
		prompt := fmt.Sprintf("\n  %s%s%s (%s)", ColorYellow, params[i].Name, ColorReset, params[i].Type)
		if params[i].Required {
			prompt += " *"
		}
		fmt.Println(prompt)

		if len(params[i].Values) > 0 {
			for j, val := range params[i].Values {
				fmt.Printf("    [%d] %s\n", j+1, val)
			}
			fmt.Printf("  %s> %s", ColorYellow, ColorReset)
			sel := readLine()
			selIdx := 0
			fmt.Sscanf(sel, "%d", &selIdx)
			if selIdx > 0 && selIdx <= len(params[i].Values) {
				params[i].Input = params[i].Values[selIdx-1]
			} else {
				params[i].Input = sel
			}
		} else {
			fmt.Printf("  %s> %s", ColorYellow, ColorReset)
			params[i].Input = readLine()
		}
	}

	return params
}

func buildActionPayload(params []ActionParam) string {
	payload := make(map[string]interface{})
	for _, p := range params {
		if p.Input != "" {
			payload[p.Name] = p.Input
		}
	}
	if len(payload) == 0 {
		return "{}"
	}
	data, _ := json.MarshalIndent(payload, "", "  ")
	return string(data)
}

func executeAction(session *InteractiveSession, actionTarget string, payload string) {
	fmt.Printf("\n%sConfirm Action%s\n", ColorYellow, ColorReset)
	fmt.Printf("  Target:  %s\n", actionTarget)
	fmt.Printf("  Payload: %s\n\n", payload)
	fmt.Printf("  Execute? (yes/no): %s> %s", ColorYellow, ColorReset)

	confirm := readLine()
	if strings.ToLower(confirm) != "yes" {
		fmt.Println("Action cancelled.")
		promptEnter()
		return
	}

	fullURL := actionTarget
	if !strings.HasPrefix(fullURL, "http") {
		if session.client.useHTTP {
			fullURL = "http://" + session.client.target + actionTarget
		} else {
			fullURL = "https://" + session.client.target + actionTarget
		}
	}

	req, err := http.NewRequest("POST", fullURL, bytes.NewBufferString(payload))
	if err != nil {
		fmt.Printf("%sError: %v%s\n", ColorRed, err, ColorReset)
		promptEnter()
		return
	}

	headers := session.client.buildAuthHeaders()
	headers["Content-Type"] = "application/json"
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := session.client.http.Do(req)
	if err != nil {
		fmt.Printf("%sError: %v%s\n", ColorRed, err, ColorReset)
		promptEnter()
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("\n%sStatus:%s %d\n%sResponse:%s\n%s\n", ColorCyan, ColorReset, resp.StatusCode, ColorCyan, ColorReset, string(body))
	promptEnter()
}

// ============================================================================
// Interactive Mode - JSON Viewer & CLI Commands
// ============================================================================

func viewRawJSON(session *InteractiveSession, path string) {
	node := session.cached[path]
	if node == nil || node.Raw == nil {
		fmt.Printf("%sNo data available%s\n", ColorRed, ColorReset)
		promptEnter()
		return
	}

	data, err := json.MarshalIndent(node.Raw, "", "  ")
	if err != nil {
		fmt.Printf("%sError formatting JSON: %v%s\n", ColorRed, err, ColorReset)
		promptEnter()
		return
	}

	tmpFile, err := os.CreateTemp("", "onefish-*.json")
	if err != nil {
		fmt.Printf("%sError: %v%s\n", ColorRed, err, ColorReset)
		promptEnter()
		return
	}
	defer os.Remove(tmpFile.Name())

	tmpFile.Write(data)
	tmpFile.Close()

	cmd := exec.Command("less", tmpFile.Name())
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Run()
}

func showCLICommands(session *InteractiveSession) {
	var commands []string

	baseCmd := fmt.Sprintf("onefish -target %s", session.client.target)
	if session.client.username != "" {
		baseCmd += fmt.Sprintf(" -u %s", session.client.username)
	}

	commands = append(commands, "# OneFish CLI Commands")
	commands = append(commands, "# ====================")
	commands = append(commands, "")
	commands = append(commands, "# Basic query")
	commands = append(commands, baseCmd)
	commands = append(commands, "")

	for i, path := range session.history {
		marker := ""
		if i == len(session.history)-1 {
			marker = " (current)"
		}
		commands = append(commands, fmt.Sprintf("# [%d] %s%s", i+1, truncatePath(path, 50), marker))
		commands = append(commands, generateCLICommand(session, path, "", ""))
		commands = append(commands, "")

		if node := session.cached[path]; node != nil {
			for name, target := range node.Actions {
				cmd := fmt.Sprintf("onefish -target %s", session.client.target)
				if session.client.username != "" {
					cmd += fmt.Sprintf(" -u %s", session.client.username)
				}
				cmd += fmt.Sprintf(" --action %s --data '{}'", target)
				commands = append(commands, fmt.Sprintf("# Action: %s", name))
				commands = append(commands, cmd)
				commands = append(commands, "")
			}
		}
	}

	tmpFile, err := os.CreateTemp("", "onefish-*.txt")
	if err != nil {
		fmt.Printf("%sError: %v%s\n", ColorRed, err, ColorReset)
		promptEnter()
		return
	}
	defer os.Remove(tmpFile.Name())

	for _, cmd := range commands {
		tmpFile.WriteString(cmd + "\n")
	}
	tmpFile.Close()

	cmd := exec.Command("less", tmpFile.Name())
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Run()
}

func generateCLICommand(session *InteractiveSession, path string, action string, data string) string {
	var cmd strings.Builder
	cmd.WriteString("onefish")
	cmd.WriteString(fmt.Sprintf(" -target %s", session.client.target))
	if session.client.username != "" {
		if session.hideCreds {
			cmd.WriteString(" -u [REDACTED]")
		} else {
			cmd.WriteString(fmt.Sprintf(" -u %s", session.client.username))
		}
	}
	if session.client.password != "" {
		cmd.WriteString(" -p [REDACTED]")
	}
	if session.client.insecure {
		cmd.WriteString(" -k")
	}
	if path != "" && path != "/redfish/v1/" {
		cmd.WriteString(fmt.Sprintf(" %s", path))
	}
	if action != "" {
		cmd.WriteString(fmt.Sprintf(" --action %s", action))
	}
	if data != "" && data != "{}" {
		cmd.WriteString(fmt.Sprintf(" --data '%s'", data))
	}
	return cmd.String()
}

// ============================================================================
// Interactive Mode - Main Loop
// ============================================================================

func runInteractiveMode(session *InteractiveSession) {
	for {
		clearScreen()

		currentPath := session.history[len(session.history)-1]
		endpoint, err := session.fetchEndpoint(currentPath)
		if err != nil {
			fmt.Printf("%sError: %v%s\n", ColorRed, err, ColorReset)
			fmt.Printf("\n%sPress Enter to go back...%s", ColorYellow, ColorReset)
			reader.ReadString('\n')
			if len(session.history) > 1 {
				session.history = session.history[:len(session.history)-1]
			}
			continue
		}

		displayEndpointInfo(endpoint)
		options := buildMenu(session, endpoint)
		displayMenu(options)
		fmt.Println()
		displayEndpointData(endpoint)

		opt, ok := readChoice(options)
		if !ok {
			fmt.Printf("\n%sInvalid option%s", ColorRed, ColorReset)
			time.Sleep(500 * time.Millisecond)
			continue
		}

		switch opt.OptType {
		case "back":
			session.history = session.history[:len(session.history)-1]
		case "member", "link":
			session.history = append(session.history, opt.Path)
		case "action":
			handleAction(session, opt.Path, opt.ActionData)
		case "json":
			viewRawJSON(session, opt.ParentPath)
		case "cli":
			showCLICommands(session)
		case "exit":
			fmt.Println("\nGoodbye!")
			return
		}
	}
}

// ============================================================================
// CLI Action Execution (non-interactive)
// ============================================================================

func executeActionFromCLI(client *Client, action string, data string) {
	fullURL := action
	if !strings.HasPrefix(fullURL, "http") {
		if client.useHTTP {
			fullURL = "http://" + client.target + action
		} else {
			fullURL = "https://" + client.target + action
		}
	}

	req, err := http.NewRequest("POST", fullURL, bytes.NewBufferString(data))
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}

	headers := client.buildAuthHeaders()
	headers["Content-Type"] = "application/json"
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.http.Do(req)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("Status: %d\nResponse:\n%s\n", resp.StatusCode, string(body))
}

// ============================================================================
// Main
// ============================================================================

func main() {
	target := flag.String("target", "", "BMC IP address and port")
	username := flag.String("u", "", "Username")
	password := flag.String("p", "", "Password")
	rateLimit := flag.String("rate-limit", "0s", "Rate limit delay between requests (e.g., 1s, 500ms). Use '0s' to disable.")
	outputFormat := flag.String("output", "tree", "Output format: tree, json")
	crawl := flag.Bool("crawl", false, "Crawl the Redfish API tree")
	interactive := flag.Bool("i", false, "Run in interactive mode")
	action := flag.String("action", "", "Execute action (path)")
	data := flag.String("data", "", "JSON payload for action")
	insecure := flag.Bool("k", false, "Skip TLS certificate verification (for self-signed certs)")
	hideCreds := flag.Bool("hide-creds", false, "Redact credentials in CLI command output")

	flag.Parse()

	if *target == "" {
		fmt.Println("Usage: onefish -target <ip:port> [-u <username> -p <password>] [-i] [--crawl] [-k]")
		return
	}

	client := NewClient(*target, *username, *password, *insecure)

	if err := client.Authenticate(); err != nil {
		fmt.Printf("Authentication failed: %v\n", err)
		return
	}

	fmt.Println("Authentication successful!")

	if *action != "" {
		executeActionFromCLI(client, *action, *data)
		return
	}

	var rl *RateLimiter
	if *rateLimit != "0s" {
		var err error
		rl, err = NewRateLimiter(*rateLimit)
		if err != nil {
			fmt.Printf("Failed to create rate limiter: %v\n", err)
			return
		}
	}

	if *crawl {
		crawler := NewCrawler(client, rl)

		fmt.Println("\nCrawling Redfish API tree...")
		start := time.Now()

		root, err := crawler.Crawl("/redfish/v1/")
		if err != nil {
			fmt.Printf("Crawl failed: %v\n", err)
			return
		}

		elapsed := time.Since(start)

		fmt.Printf("Crawl completed in %v\n", elapsed)
		fmt.Printf("Visited %d endpoints\n", len(crawler.visited))

		fmt.Println("\n=== Redfish API Tree ===")
		printTree(root, "", true)

		if *outputFormat == "json" {
			fmt.Println("\n=== Raw JSON ===")
			output, err := json.MarshalIndent(root, "", "  ")
			if err != nil {
				fmt.Printf("Failed to format output: %v\n", err)
				return
			}
			fmt.Println(string(output))
		}
	} else if *interactive {
		session := NewInteractiveSession(client, rl, *hideCreds)
		session.history = append(session.history, "/redfish/v1/")
		runInteractiveMode(session)
	} else {
		resp, err := client.Get("/redfish/v1/")
		if err != nil {
			fmt.Printf("Request failed: %v\n", err)
			return
		}
		defer resp.Body.Close()

		fmt.Printf("Status: %d\n", resp.StatusCode)

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			fmt.Printf("Failed to read response body: %v\n", err)
			return
		}

		fmt.Printf("Response:\n%s\n", string(body))
	}
}
