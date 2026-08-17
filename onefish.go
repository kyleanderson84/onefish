package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type Client struct {
	target   string
	username string
	password string
	token    string
	useHTTP  bool
}

func NewClient(target, username, password string) *Client {
	return &Client{
		target:   target,
		username: username,
		password: password,
	}
}

func (c *Client) Authenticate() error {
	// If no credentials provided, skip authentication
	if c.username == "" && c.password == "" {
		return nil
	}

	// Try basic auth first
	if err := c.tryBasicAuth(); err == nil {
		return nil
	}

	// Fall back to session token authentication
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

	// If we get a TLS handshake error, the endpoint is probably plain HTTP.
	// Fall back to HTTP.
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

func main() {
	target := flag.String("target", "", "BMC IP address and port")
	username := flag.String("u", "", "Username")
	password := flag.String("p", "", "Password")

	flag.Parse()

	if *target == "" {
		fmt.Println("Usage: onefish -target <ip:port> [-u <username> -p <password>]")
		return
	}

	client := NewClient(*target, *username, *password)

	if err := client.Authenticate(); err != nil {
		fmt.Printf("Authentication failed: %v\n", err)
		return
	}

	fmt.Println("Authentication successful!")
	
	// Example usage of the client
	resp, err := client.Get("/redfish/v1/")
	if err != nil {
		fmt.Printf("Request failed: %v\n", err)
		return
	}
	defer resp.Body.Close()
	
	fmt.Printf("Status: %d\n", resp.StatusCode)
	
	// Print the response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Printf("Failed to read response body: %v\n", err)
		return
	}
	
	fmt.Printf("Response:\n%s\n", string(body))
}
