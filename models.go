package main

import (
	"bufio"
	"log"
	"os"
	"strings"
)

// loadEnvFile loads environment variables from .env file if it exists
func loadEnvFile() {
	envFile, err := os.Open(".env")
	if err != nil {
		// .env file not found, skip loading
		return
	}
	defer envFile.Close()

	scanner := bufio.NewScanner(envFile)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Parse KEY=VALUE
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])
			// Remove surrounding quotes if present
			value = strings.Trim(value, `"`)
			os.Setenv(key, value)
		}
	}
}

// Config holds all configuration from environment variables
type Config struct {
	Port                 string
	ClickUpAPIToken      string
	ClickUpListCTI       string
	ClickUpListCloudSec  string
	ClickUpListSOC       string
	ClickUpAssigneeCTI   string
	ClickUpAssigneeCloudSec string
	ClickUpAssigneeSOC   string
	TeamsHMACSecret     string
}

// LoadConfig creates Config from environment variables
func LoadConfig() *Config {
	// Load .env file if present
	loadEnvFile()

	config := &Config{
		Port:                 getEnv("PORT", "8080"),
		ClickUpAPIToken:      getEnv("CLICKUP_API_TOKEN", ""),
		ClickUpListCTI:       getEnv("CLICKUP_LIST_CTI", ""),
		ClickUpListCloudSec:  getEnv("CLICKUP_LIST_CLOUDSEC", ""),
		ClickUpListSOC:       getEnv("CLICKUP_LIST_SOC", ""),
		ClickUpAssigneeCTI:   getEnv("CLICKUP_ASSIGNEE_CTI", ""),
		ClickUpAssigneeCloudSec: getEnv("CLICKUP_ASSIGNEE_CLOUDSEC", ""),
		ClickUpAssigneeSOC:   getEnv("CLICKUP_ASSIGNEE_SOC", ""),
		TeamsHMACSecret:      getEnv("TEAMS_HMAC_SECRET", ""),
	}

	// Validate required configuration
	if config.ClickUpAPIToken == "" {
		log.Fatal("CLICKUP_API_TOKEN is required")
	}
	if config.TeamsHMACSecret == "" {
		log.Fatal("TEAMS_HMAC_SECRET is required")
	}
	if config.ClickUpListCTI == "" {
		log.Fatal("CLICKUP_LIST_CTI is required")
	}
	if config.ClickUpListCloudSec == "" {
		log.Fatal("CLICKUP_LIST_CLOUDSEC is required")
	}
	if config.ClickUpListSOC == "" {
		log.Fatal("CLICKUP_LIST_SOC is required")
	}

	return config
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// TeamsWebhookPayload represents the incoming payload from Microsoft Teams
type TeamsWebhookPayload struct {
	Type        string             `json:"type"`
	Text        string             `json:"text"`
	From        TeamsUser          `json:"from"`
	ChannelID   string             `json:"channelId"`
	ChannelData *TeamsChannelData  `json:"channelData"`
}

// TeamsUser represents the user who sent the message
type TeamsUser struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

// TeamsChannelData contains channel information
type TeamsChannelData struct {
	Channel *TeamsChannel `json:"channel"`
}

// TeamsChannel represents a Teams channel
type TeamsChannel struct {
	ID string `json:"id"`
}

// TeamsResponse is the response sent back to Microsoft Teams
type TeamsResponse struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ClickUpTaskRequest represents the request to create a task in ClickUp
type ClickUpTaskRequest struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	Assignees   []string `json:"assignees"`
}

// ClickUpTaskResponse represents the response from ClickUp API
type ClickUpTaskResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

// ClickUpErrorResponse represents an error from ClickUp API
type ClickUpErrorResponse struct {
	Err   string `json:"err"`
	ECode int    `json:"ECode"`
}
