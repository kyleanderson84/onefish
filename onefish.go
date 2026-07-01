package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
)

type Client struct {
	target   string
	username string
	password string
	token    string
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

func (c *Client) tryBasicAuth() error {
	url := fmt.Sprintf("https://%s/redfish/v1/", c.target)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}

	// Only set basic auth if credentials are provided
	if c.username != "" && c.password != "" {
		req.SetBasicAuth(c.username, c.password)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
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
	url := fmt.Sprintf("https://%s/redfish/v1/SessionService/Sessions", c.target)

	sessionData := map[string]string{
		"UserName": c.username,
		"Password": c.password,
	}

	jsonData, err := json.Marshal(sessionData)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
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
	fullURL := fmt.Sprintf("https://%s%s", c.target, url)
	
	req, err := http.NewRequest("GET", fullURL, nil)
	if err != nil {
		return nil, err
	}

	// Only add auth headers if credentials are provided
	if c.username != "" && c.password != "" {
		if c.token != "" {
			req.Header.Set("X-Auth-Token", c.token)
		} else {
			req.SetBasicAuth(c.username, c.password)
		}
	}

	client := &http.Client{}
	return client.Do(req)
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
}
