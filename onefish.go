package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
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

func NewClient(target, username, password string) *Client {
	return &Client{
		target:   target,
		username: username,
		password: password,
	}
}

func (c *Client) Authenticate() error {
	if c.username == "" && c.password == "" {
		return nil
	}

	if err := c.tryBasicAuth(); err == nil {
		return nil
	}

	return c.createSession()
}

func (c *Client) doRequest(method, url string, body io.Reader, headers map[string]string) (*http.Response, error) {
	fullURL := fmt.Sprintf("https://%s%s", c.target, url)

	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, err
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	client := &http.Client{}
	resp, err := client.Do(req)

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

		resp, err = client.Do(req)
	}

	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (c *Client) tryBasicAuth() error {
	headers := map[string]string{}
	if c.username != "" && c.password != "" {
		req, err := http.NewRequest("GET", "http://example.com", nil)
		if err != nil {
			return err
		}
		req.SetBasicAuth(c.username, c.password)
		headers["Authorization"] = req.Header.Get("Authorization")
	}

	resp, err := c.doRequest("GET", "/redfish/v1/", nil, headers)
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

	resp, err := c.doRequest("POST", "/redfish/v1/SessionService/Sessions", bytes.NewBuffer(jsonData), map[string]string{"Content-Type": "application/json"})
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
	headers := map[string]string{}
	if c.username != "" && c.password != "" {
		if c.token != "" {
			headers["X-Auth-Token"] = c.token
		} else {
			req, err := http.NewRequest("GET", "http://example.com", nil)
			if err != nil {
				return nil, err
			}
			req.SetBasicAuth(c.username, c.password)
			headers["Authorization"] = req.Header.Get("Authorization")
		}
	}

	return c.doRequest("GET", url, nil, headers)
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
		ID:    url,
		Raw:   data,
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

				resp, err := c.client.GetWithRateLimit(fullID, c.rateLimiter)
				if err != nil {
					continue
				}
				defer resp.Body.Close()

				body, err := io.ReadAll(resp.Body)
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

				resp, err := c.client.GetWithRateLimit(fullID, c.rateLimiter)
				if err != nil {
					continue
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

func main() {
	target := flag.String("target", "", "BMC IP address and port")
	username := flag.String("u", "", "Username")
	password := flag.String("p", "", "Password")
	rateLimit := flag.String("rate-limit", "1s", "Rate limit delay between requests (e.g., 1s, 500ms). Use '0s' or 'no-rate-limit' to disable.")
	outputFormat := flag.String("output", "tree", "Output format: tree, json")
	crawl := flag.Bool("crawl", false, "Crawl the Redfish API tree")

	flag.Parse()

	if *target == "" {
		fmt.Println("Usage: onefish -target <ip:port> [-u <username> -p <password>] [--crawl]")
		return
	}

	client := NewClient(*target, *username, *password)

	if err := client.Authenticate(); err != nil {
		fmt.Printf("Authentication failed: %v\n", err)
		return
	}

	fmt.Println("Authentication successful!")

	if *crawl {
		rl := (*RateLimiter)(nil)
		if *rateLimit != "0s" && *rateLimit != "no-rate-limit" {
			var err error
			rl, err = NewRateLimiter(*rateLimit)
			if err != nil {
				fmt.Printf("Failed to create rate limiter: %v\n", err)
				return
			}
		}

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
