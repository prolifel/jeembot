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
	TeamsAppID          string
	TeamsAppSecret      string
	TeamsTenantID       string
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
		TeamsAppID:           getEnv("TEAMS_APP_ID", ""),
		TeamsAppSecret:       getEnv("TEAMS_APP_SECRET", ""),
		TeamsTenantID:        getEnv("TEAMS_TENANT_ID", ""),
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

	// Bot Framework configuration (required for /api/messages endpoint)
	if config.TeamsAppID == "" {
		log.Println("[WARN] TEAMS_APP_ID not set - Bot Framework endpoint will be disabled")
	}
	if config.TeamsAppSecret == "" {
		log.Println("[WARN] TEAMS_APP_SECRET not set - Bot Framework endpoint will be disabled")
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
	Type        string       `json:"type"`
	Text        string       `json:"text"`
	Attachments []Attachment `json:"attachments,omitempty"`
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

// --- Bot Framework Activity Types ---

// Activity represents a Bot Framework activity
// https://docs.microsoft.com/en-us/azure/bot-service/rest-api/bot-framework-rest-connector-api-reference?view=azure-bot-service-4.0#activity-object
type Activity struct {
	Type         string           `json:"type"`
	ID           string           `json:"id"`
	Timestamp    string           `json:"timestamp"`
	ChannelID    string           `json:"channelId"`
	ServiceURL   string           `json:"serviceUrl"`
	From         *ChannelAccount  `json:"from"`
	Conversation *ConversationAccount `json:"conversation"`
	Recipient    *ChannelAccount  `json:"recipient"`
	TextFormat   string           `json:"textFormat"`
	Text         string           `json:"text"`
	Attachments  []Attachment     `json:"attachments"`
	Entities     []Entity         `json:"entities"`
	ChannelData  interface{}      `json:"channelData"`
	MembersAdded []*ChannelAccount `json:"membersAdded"`
}

// ChannelAccount represents a user or bot
type ChannelAccount struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ConversationAccount represents a conversation
type ConversationAccount struct {
	ID string `json:"id"`
}

// Attachment represents a message attachment
type Attachment struct {
	ContentType string      `json:"contentType"`
	Content     interface{} `json:"content"`
}

// Entity represents an entity (e.g., mention)
type Entity struct {
	Type   string                 `json:"type"`
	Properties map[string]interface{} `json:"properties"`
}

// ActivityResponse represents a Bot Framework activity response
type ActivityResponse struct {
	Type         string           `json:"type"`
	Text         string           `json:"text"`
	From         *ChannelAccount  `json:"from"`
	Recipient    *ChannelAccount  `json:"recipient"`
	Conversation *ConversationAccount `json:"conversation"`
	ChannelID    string           `json:"channelId"`
	Attachments  []Attachment     `json:"attachments"`
}

// --- Adaptive Card Types ---

// AdaptiveCard represents a Microsoft Adaptive Card
type AdaptiveCard struct {
	Type       string          `json:"type"`
	Version    string          `json:"version"`
	Body       []CardElement   `json:"body"`
	Actions    []CardAction    `json:"actions,omitempty"`
	Schema     string          `json:"$schema,omitempty"`
}

// CardElement represents an element in an Adaptive Card
type CardElement struct {
	Type     string         `json:"type"`
	Text     string         `json:"text,omitempty"`
	URL      string         `json:"url,omitempty"`
	Title    string         `json:"title,omitempty"`
	Style    string         `json:"style,omitempty"`
	Size     string         `json:"size,omitempty"`
	Weight   string         `json:"weight,omitempty"`
	Color    string         `json:"color,omitempty"`
	Items    []CardElement  `json:"items,omitempty"`
	Columns  []CardColumn   `json:"columns,omitempty"`
	Facts    []Fact         `json:"facts,omitempty"`
	Spacing  string         `json:"spacing,omitempty"`
	HorizontalAlignment string `json:"horizontalAlignment,omitempty"`
	IsSubtle bool           `json:"isSubtle,omitempty"`
	Wrap     bool           `json:"wrap,omitempty"`
}

// CardColumnSet represents a column set in an Adaptive Card
type CardColumnSet struct {
	Type     string        `json:"type"`
	Columns  []CardColumn  `json:"columns"`
}

// Fact represents a key-value pair in a FactSet
type Fact struct {
	Title string `json:"title"`
	Value string `json:"value"`
}

// CardColumn represents a column in a column set
type CardColumn struct {
	Type    string        `json:"type"`
	Width   string        `json:"width"`
	Items   []CardElement `json:"items"`
}

// CardAction represents an action in an Adaptive Card
type CardAction struct {
	Type  string `json:"type"`
	Title string `json:"title"`
	URL   string `json:"url,omitempty"`
	// For Bot Framework, use msteams property for Teams-specific actions
	MSTeams *MSTeamsAction `json:"msteams,omitempty"`
}

// MSTeamsAction represents Teams-specific action properties
type MSTeamsAction struct {
	Type string `json:"type"`
}
