package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/golang-jwt/jwt/v5"
)

// Handler holds dependencies for HTTP handlers
type Handler struct {
	clickup *ClickUpService
	config  *Config
}

// NewHandler creates a new handler instance
func NewHandler(config *Config) *Handler {
	return &Handler{
		clickup: NewClickUpService(config),
		config:  config,
	}
}

// TeamsWebhook handles incoming Teams outgoing webhook requests
func (h *Handler) TeamsWebhook(w http.ResponseWriter, r *http.Request) {
	// Debug log incoming request
	log.Printf("[DEBUG] Incoming request from %s - Method: %s", r.RemoteAddr, r.Method)

	// Read body for HMAC validation
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("[ERROR] Error reading body: %v", err)
		h.sendResponse(w, "No message received")
		return
	}

	log.Printf("[DEBUG] Request body: %s", string(body))

	// Validate HMAC
	if !h.validateHMAC(r.Header.Get("Authorization"), string(body)) {
		log.Printf("[WARN] HMAC validation failed from %s", r.RemoteAddr)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	log.Printf("[DEBUG] HMAC validation passed")

	// Parse payload
	var payload TeamsWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		log.Printf("[ERROR] Error parsing payload: %v", err)
		h.sendResponse(w, "Invalid message format")
		return
	}

	log.Printf("[DEBUG] Parsed payload from user: %s", payload.From.Name)

	// Parse command
	listName, taskDetail, err := parseCommand(payload.Text)
	if err != nil {
		log.Printf("[WARN] Command parse error: %v", err)
		h.sendResponse(w, err.Error())
		return
	}

	log.Printf("[DEBUG] Parsed command - List: %s, Task: %s", listName, taskDetail)

	// Get list ID
	listID, ok := h.clickup.GetListID(listName)
	if !ok {
		log.Printf("[WARN] Invalid list: %s", listName)
		h.sendResponse(w, "Invalid list. Use: cti, cloudsec, or soc\nExample: jeembot /to cti Your task description")
		return
	}

	// Create task in ClickUp
	task, err := h.clickup.CreateTask(listName, listID, taskDetail, "")
	if err != nil {
		log.Printf("[ERROR] Error creating task: %v", err)
		h.sendResponse(w, "Failed to create task in ClickUp. Please try again or create manually.")
		return
	}

	log.Printf("[INFO] Task created successfully: %s (ID: %s)", task.Name, task.ID)

	// Send success response
	listDisplay := strings.ToUpper(listName)
	h.sendResponse(w, fmt.Sprintf("Task created in ClickUp: '%s' (List: %s)\n%s", task.Name, listDisplay, task.URL))
}

// BotMessages handles incoming Bot Framework messages from Microsoft Teams
// This endpoint is used by the full Teams App (not outgoing webhook)
func (h *Handler) BotMessages(w http.ResponseWriter, r *http.Request) {
	log.Printf("[DEBUG] Bot Framework message from %s - Method: %s", r.RemoteAddr, r.Method)

	// Validate HTTP method
	if r.Method != http.MethodPost {
		log.Printf("[WARN] Invalid method: %s", r.Method)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Check if Bot Framework is configured
	if h.config.TeamsAppID == "" || h.config.TeamsAppSecret == "" {
		log.Printf("[WARN] Bot Framework not configured - missing TEAMS_APP_ID or TEAMS_APP_SECRET")
		http.Error(w, "Bot Framework not configured", http.StatusServiceUnavailable)
		return
	}

	// Read body for JWT validation
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("[ERROR] Error reading body: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	log.Printf("[DEBUG] Bot message body: %s", string(body))

	// Validate JWT token
	if !h.validateJWT(r.Header.Get("Authorization")) {
		log.Printf("[WARN] JWT validation failed from %s", r.RemoteAddr)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	log.Printf("[DEBUG] JWT validation passed")

	// Parse activity
	var activity Activity
	if err := json.Unmarshal(body, &activity); err != nil {
		log.Printf("[ERROR] Error parsing activity: %v", err)
		h.sendBotResponse(w, "Invalid message format", &activity)
		return
	}

	fromName := "unknown"
	if activity.From != nil {
		fromName = activity.From.Name
	}
	log.Printf("[DEBUG] Activity type: %s, From: %s", activity.Type, fromName)

	// Handle different activity types
	switch activity.Type {
	case "message":
		h.handleBotMessage(w, &activity)
	case "conversationUpdate":
		// Check if bot was added to conversation
		if h.handleBotAdded(w, &activity) {
			return
		}
		w.WriteHeader(http.StatusOK)
	case "installationUpdate":
		// Handle bot installation/uninstallation in Teams
		// This is sent when bot is added/removed from a team or chat
		log.Printf("[DEBUG] InstallationUpdate activity received")
		w.WriteHeader(http.StatusOK)
	case "ping":
		// Handle ping from Bot Framework
		log.Printf("[DEBUG] Ping activity - responding OK")
		w.WriteHeader(http.StatusOK)
	default:
		log.Printf("[DEBUG] Unknown activity type: %s", activity.Type)
		w.WriteHeader(http.StatusOK)
	}
}

// handleBotAdded processes when bot is added to a conversation
func (h *Handler) handleBotAdded(w http.ResponseWriter, activity *Activity) bool {
	// Check if membersAdded contains our bot
	if activity.MembersAdded == nil {
		return false
	}

	botID := h.config.TeamsAppID
	for _, member := range activity.MembersAdded {
		if member.ID == botID {
			// Bot was added - send welcome message with Adaptive Card
			log.Printf("[INFO] Bot added to conversation, sending welcome message")
			h.sendBotResponse(w, "Welcome to Jeembot! 🎉\n\nI help you create tasks in ClickUp directly from Microsoft Teams.\n\nAvailable commands:\n• /to cti <task> - Create task in CTI list\n• /to cloudsec <task> - Create task in CloudSec list\n• /to soc <task> - Create task in SOC list\n\nExample: /to cti Fix login bug\n\nJust type your task and I'll create it for you!", activity)
			return true
		}
	}
	return false
}

// stripMentionTags removes Teams mention tags like <at>jeembot</at> from message text
func stripMentionTags(text string) string {
	// Remove <at>...</at> pattern used by Teams for bot mentions
	re := regexp.MustCompile(`<at[^>]*>.*?</at>`)
	return re.ReplaceAllString(text, "")
}

// handleBotMessage processes a message activity from Teams
func (h *Handler) handleBotMessage(w http.ResponseWriter, activity *Activity) {
	// Strip Teams mention tags (e.g., <at>jeembot</at>) from message text
	cleanText := stripMentionTags(activity.Text)

	// Check for greeting commands first
	text := strings.TrimSpace(strings.ToLower(cleanText))
	if text == "hi" || text == "hello" {
		h.sendBotResponse(w, "Hello! I'm Jeembot!\n\nI help you create tasks in ClickUp without leaving Teams.\n\nUse /to <list> <task> to create tasks:\n• /to cti <task> - CTI team\n• /to cloudsec <task> - CloudSec team\n• /to soc <task> - SOC team\n\nExample: /to cti Review security alert", activity)
		return
	}
	if text == "help" {
		h.sendBotResponse(w, "Jeembot Help\n\nCreate tasks in ClickUp using:\n/to <team> <task description>\n\nTeams:\n• cti - CTI team\n• cloudsec - CloudSec team\n• soc - SOC team\n\nExamples:\n• /to cti Update firewall rules\n• /to cloudsec Review access request\n• /to soc Investigate alert #123\n\nNeed help? Just ask!", activity)
		return
	}

	// Parse command from message text
	// For Bot Framework, the text doesn't have HTML tags like the webhook
	listName, taskDetail, err := parseBotCommand(cleanText)
	if err != nil {
		log.Printf("[WARN] Command parse error: %v", err)
		h.sendBotResponse(w, err.Error(), activity)
		return
	}

	log.Printf("[DEBUG] Parsed command - List: %s, Task: %s", listName, taskDetail)

	// Get list ID
	listID, ok := h.clickup.GetListID(listName)
	if !ok {
		log.Printf("[WARN] Invalid list: %s", listName)
		h.sendBotResponse(w, "Invalid list. Use: /to <cti|cloudsec|soc> <task detail>\nExample: /to cti Your task description", activity)
		return
	}

	// Create task in ClickUp
	task, err := h.clickup.CreateTask(listName, listID, taskDetail, "")
	if err != nil {
		log.Printf("[ERROR] Error creating task: %v", err)
		h.sendBotResponse(w, "Failed to create task in ClickUp. Please try again or create manually.", activity)
		return
	}

	log.Printf("[INFO] Task created successfully: %s (ID: %s)", task.Name, task.ID)

	// Send success response
	listDisplay := strings.ToUpper(listName)
	h.sendBotResponse(w, fmt.Sprintf("Task created in ClickUp: '%s' (List: %s)\n%s", task.Name, listDisplay, task.URL), activity)
}

// parseBotCommand parses the Bot Framework message text to extract list and task detail
// Expected format: /to <list> <task detail>
// Handles plain text format (not HTML like outgoing webhook)
func parseBotCommand(text string) (listName, taskDetail string, err error) {
	// Trim whitespace
	text = strings.TrimSpace(text)

	log.Printf("[DEBUG] Bot command text: %s", text)

	// Split by whitespace
	parts := strings.Fields(text)
	if len(parts) < 2 {
		return "", "", fmt.Errorf("Invalid syntax. Use: /to <cti|cloudsec|soc> <task detail>\nExample: /to cti Your task description")
	}

	// Check for /to command
	if parts[0] != "/to" {
		return "", "", fmt.Errorf("Invalid command. Use: /to <cti|cloudsec|soc> <task detail>")
	}

	// Validate list name
	listName = parts[1]
	if listName != "cti" && listName != "cloudsec" && listName != "soc" {
		return "", "", fmt.Errorf("Invalid list. Use: cti, cloudsec, or soc\nExample: /to cti Your task description")
	}

	// Get remaining parts as task detail
	if len(parts) < 3 {
		return "", "", fmt.Errorf("Please provide task details\nExample: /to cti Fix login bug")
	}

	taskDetail = strings.Join(parts[2:], " ")

	// Validate task detail is not empty after trimming
	taskDetail = strings.TrimSpace(taskDetail)
	if taskDetail == "" {
		return "", "", fmt.Errorf("Please provide task details")
	}

	// Validate UTF-8
	if !utf8.ValidString(taskDetail) {
		return "", "", fmt.Errorf("Invalid task details")
	}

	return listName, taskDetail, nil
}

// sendBotResponse sends a Bot Framework activity response with Adaptive Card
func (h *Handler) sendBotResponse(w http.ResponseWriter, message string, activity *Activity) {
	// For Bot Framework, we need to send the response via the Bot Service API
	// not by writing to the HTTP response

	// Create Adaptive Card
	card := createAdaptiveCard(message)

	// Build response activity - swap from/recipient for reply
	// Don't set Text when using Adaptive Card to avoid duplication
	resp := Activity{
		Type:         "message",
		TextFormat:   "plain",
		From:         activity.Recipient,
		Recipient:    activity.From,
		Conversation: activity.Conversation,
		ChannelID:    activity.ChannelID,
		ServiceURL:   activity.ServiceURL,
		Attachments: []Attachment{
			{
				ContentType: "application/vnd.microsoft.card.adaptive",
				Content:     card,
			},
		},
	}

	// Send via Bot Framework API
	if err := h.sendToTeams(&resp); err != nil {
		log.Printf("[ERROR] Failed to send bot response: %v", err)
	}

	// Still write OK to the original response
	w.WriteHeader(http.StatusOK)
}

// sendToTeams sends an activity to Teams via the Bot Framework API
func (h *Handler) sendToTeams(activity *Activity) error {
	if activity.ServiceURL == "" {
		return fmt.Errorf("no service URL available")
	}

	// Get OAuth token for Bot Framework
	token, err := h.getBotToken()
	if err != nil {
		return fmt.Errorf("failed to get bot token: %w", err)
	}

	// Build the Bot Framework API URL - send to conversation activities endpoint
	// (not reply to specific activity)
	serviceURL := strings.TrimRight(activity.ServiceURL, "/")
	url := fmt.Sprintf("%s/v3/conversations/%s/activities",
		serviceURL,
		activity.Conversation.ID)

	log.Printf("[DEBUG] Sending response to Teams: %s", url)

	// Create HTTP client with timeout
	client := &http.Client{Timeout: 10 * time.Second}

	// Encode the activity
	body, err := json.Marshal(activity)
	if err != nil {
		return fmt.Errorf("failed to marshal activity: %w", err)
	}

	// Create request
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf(" Teams API returned status %d: %s", resp.StatusCode, string(respBody))
	}

	log.Printf("[DEBUG] Response sent to Teams successfully")
	return nil
}

// getBotToken obtains an OAuth token from Azure AD for Bot Framework
func (h *Handler) getBotToken() (string, error) {
	// Determine token URL based on tenant configuration
	var tokenURL string
	if h.config.TeamsTenantID != "" {
		// Single tenant: use specific Azure AD tenant
		tokenURL = fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", h.config.TeamsTenantID)
	} else {
		// Multi-tenant: use botframework.com
		tokenURL = "https://login.microsoftonline.com/botframework.com/oauth2/v2.0/token"
	}

	// Create form data for token request
	formData := url.Values{}
	formData.Set("grant_type", "client_credentials")
	formData.Set("client_id", h.config.TeamsAppID)
	formData.Set("client_secret", h.config.TeamsAppSecret)
	formData.Set("scope", "https://api.botframework.com/.default")

	log.Printf("[DEBUG] Getting bot OAuth token")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.PostForm(tokenURL, formData)
	if err != nil {
		return "", fmt.Errorf("failed to get token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("token request failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	// Parse token response
	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", fmt.Errorf("failed to parse token response: %w", err)
	}

	if tokenResp.AccessToken == "" {
		return "", fmt.Errorf("no access token in response")
	}

	log.Printf("[DEBUG] Got bot token (expires in %d seconds)", tokenResp.ExpiresIn)
	return tokenResp.AccessToken, nil
}

// validateHMAC validates the HMAC signature from the Authorization header
// Following Microsoft Teams outgoing webhook validation pattern
func (h *Handler) validateHMAC(authHeader, body string) bool {
	// Check if header is present
	if authHeader == "" {
		log.Println("[WARN] Missing Authorization header")
		return false
	}

	// Extract signature - support both "HMAC <signature>" and just "<signature>"
	var signature string
	if strings.HasPrefix(strings.ToUpper(authHeader), "HMAC ") {
		signature = strings.TrimPrefix(authHeader, "HMAC ")
	} else {
		signature = authHeader
	}

	if signature == "" {
		log.Println("[WARN] Empty HMAC signature")
		return false
	}

	// Compute expected HMAC using Base64-encoded secret
	calculatedHmac := computeHMAC(body, h.config.TeamsHMACSecret)

	// Use constant-time comparison to prevent timing attacks
	if hmac.Equal([]byte(calculatedHmac), []byte(signature)) {
		return true
	}

	log.Printf("[WARN] HMAC mismatch. Expected: %s, Provided: %s", calculatedHmac, signature)
	return false
}

// computeHMAC computes HMAC-SHA256 of the message using Base64-decoded secret
// Returns Base64-encoded hash to match Teams webhook format
func computeHMAC(message, secret string) string {
	// Decode the Base64-encoded secret
	keyBytes, err := base64.StdEncoding.DecodeString(secret)
	if err != nil {
		log.Printf("[ERROR] Failed to decode HMAC secret: %v", err)
		return ""
	}

	// Compute HMAC-SHA256
	h := hmac.New(sha256.New, keyBytes)
	h.Write([]byte(message))

	// Return Base64-encoded hash (matching Teams format)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// cleanHTML removes HTML tags and decodes common HTML entities
// Handles Teams webhook HTML format: "<p><at>jeembot</at>&nbsp;/to cloudsec hello world</p>"
func cleanHTML(text string) string {
	// Replace common HTML entities
	text = strings.ReplaceAll(text, "&nbsp;", " ")
	text = strings.ReplaceAll(text, "&amp;", "&")
	text = strings.ReplaceAll(text, "&lt;", "<")
	text = strings.ReplaceAll(text, "&gt;", ">")
	text = strings.ReplaceAll(text, "&quot;", "\"")
	text = strings.ReplaceAll(text, "&#39;", "'")

	// Remove HTML tags using simple replacement
	// This handles <p>, </p>, <at>, </at>, etc.
	var result strings.Builder
	result.Grow(len(text))

	i := 0
	for i < len(text) {
		if text[i] == '<' {
			// Skip until we find '>'
			for i < len(text) && text[i] != '>' {
				i++
			}
			if i < len(text) {
				i++ // skip '>'
			}
		} else {
			result.WriteByte(text[i])
			i++
		}
	}

	return result.String()
}

// parseCommand parses the Teams message text to extract list and task detail
// Expected format: jeembot /to <list> <task detail>
// Handles HTML format: "<p><at>jeembot</at>&nbsp;/to cloudsec hello world</p>"
func parseCommand(text string) (listName, taskDetail string, err error) {
	// Clean HTML: strip tags and decode entities
	text = cleanHTML(text)

	log.Printf("[DEBUG] Text: %s", text)

	// Find jeembot mention (case-insensitive)
	text = strings.ToLower(text)
	mentionIdx := strings.Index(text, "jeembot")
	if mentionIdx == -1 {
		return "", "", fmt.Errorf("Invalid syntax. Use: jeembot /to <cti|cloudsec|soc> <task detail>")
	}

	// Extract text after mention
	afterMention := strings.TrimSpace(text[mentionIdx+len("jeembot"):])

	// Split by whitespace
	parts := strings.Fields(afterMention)
	if len(parts) < 2 {
		return "", "", fmt.Errorf("Invalid syntax. Use: jeembot /to <cti|cloudsec|soc> <task detail>")
	}

	// Check for /to command
	if parts[0] != "/to" {
		return "", "", fmt.Errorf("Invalid syntax. Use: jeembot /to <cti|cloudsec|soc> <task detail>")
	}

	// Validate list name
	listName = parts[1]
	if listName != "cti" && listName != "cloudsec" && listName != "soc" {
		return "", "", fmt.Errorf("Invalid list. Use: cti, cloudsec, or soc\nExample: jeembot /to cti Your task description")
	}

	// Get remaining parts as task detail
	if len(parts) < 3 {
		return "", "", fmt.Errorf("Please provide task details")
	}

	taskDetail = strings.Join(parts[2:], " ")

	// Validate task detail is not empty after trimming
	taskDetail = strings.TrimSpace(taskDetail)
	if taskDetail == "" {
		return "", "", fmt.Errorf("Please provide task details")
	}

	// Validate UTF-8
	if !utf8.ValidString(taskDetail) {
		return "", "", fmt.Errorf("Invalid task details")
	}

	return listName, taskDetail, nil
}

// sendResponse sends a Teams-compatible JSON response with Adaptive Card
func (h *Handler) sendResponse(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")

	// Create Adaptive Card for the response
	card := createAdaptiveCard(message)

	// Only send Adaptive Card without plain text to avoid duplication in Teams
	resp := TeamsResponse{
		Type:        "message",
		Attachments: []Attachment{
			{
				ContentType: "application/vnd.microsoft.card.adaptive",
				Content:     card,
			},
		},
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("Error encoding response: %v", err)
	}
}

// createAdaptiveCard creates an Adaptive Card from a message string
// It creates a nicely formatted card with proper styling
func createAdaptiveCard(message string) AdaptiveCard {
	// Detect if this is a task creation message
	isTaskCreated := strings.Contains(message, "Task created")

	// Default card structure
	card := AdaptiveCard{
		Type:    "AdaptiveCard",
		Version: "1.4",
		Schema:  "http://adaptivecards.io/schemas/adaptive-card.json",
		Body:    []CardElement{},
	}

	if isTaskCreated {
		// Parse task creation message
		// Format: "Task created in ClickUp: 'task name' (List: CTI)\nurl"
		var taskName, listName, taskURL string

		// Extract task name, list, and URL from message
		lines := strings.Split(message, "\n")
		for _, line := range lines {
			if strings.Contains(line, "Task created in ClickUp:") {
				// Extract task name from format: "Task created in ClickUp: 'Task Name' (List: XXX)"
				taskName = strings.TrimSpace(strings.TrimPrefix(line, "Task created in ClickUp:"))
				taskName = strings.Trim(taskName, "'")
				if idx := strings.Index(taskName, " (List:"); idx > 0 {
					taskName = taskName[:idx]
				}
			} else if strings.Contains(line, "List:") {
				// Extract list name
				listName = strings.TrimSpace(strings.TrimPrefix(line, "List:"))
			} else if strings.HasPrefix(line, "http") {
				taskURL = strings.TrimSpace(line)
			}
		}

		// Build well-formatted card
		card.Body = append(card.Body, CardElement{
			Type:  "Container",
			Items: []CardElement{
				{
					Type: "ColumnSet",
					Columns: []CardColumn{
						{
							Type:  "Column",
							Width: "auto",
							Items: []CardElement{
								{
									Type: "TextBlock",
									Text: "🎯",
									Size: "large",
								},
							},
						},
						{
							Type:  "Column",
							Width: "stretch",
							Items: []CardElement{
								{
									Type:             "TextBlock",
									Text:             "Task Created",
									Weight:           "bolder",
									Size:             "medium",
									Color:            "good",
									HorizontalAlignment: "left",
								},
								{
									Type:             "TextBlock",
									Text:             "Your task has been created in ClickUp",
									IsSubtle:         true,
									HorizontalAlignment: "left",
									Size:             "small",
								},
							},
						},
					},
				},
			},
		})

		// Add task details in a styled container
		card.Body = append(card.Body, CardElement{
			Type: "Container",
			Style: "emphasis",
			Items: []CardElement{
				{
					Type:  "TextBlock",
					Text:  "📝 " + taskName,
					Wrap:  true,
					Weight: "bolder",
					Size:  "medium",
				},
				{
					Type:  "FactSet",
					Facts: []Fact{
						{Title: "List:", Value: listName},
					},
				},
			},
		})

		// Add action button if URL is available
		if taskURL != "" {
			card.Actions = []CardAction{
				{
					Type:  "Action.OpenUrl",
					Title: "🔗 Open in ClickUp",
					URL:   taskURL,
				},
			}
		}
	} else if strings.Contains(message, "Welcome") || strings.Contains(message, "help") {
		// Help/Welcome message card
		card.Body = append(card.Body, CardElement{
			Type: "Container",
			Items: []CardElement{
				{
					Type: "ColumnSet",
					Columns: []CardColumn{
						{
							Type:  "Column",
							Width: "auto",
							Items: []CardElement{
								{
									Type: "TextBlock",
									Text: "👋",
									Size: "large",
								},
							},
						},
						{
							Type:  "Column",
							Width: "stretch",
							Items: []CardElement{
								{
									Type:             "TextBlock",
									Text:             "Welcome to Jeembot",
									Weight:           "bolder",
									Size:             "large",
									HorizontalAlignment: "left",
								},
							},
						},
					},
				},
				{
					Type:  "TextBlock",
					Text:  message,
					Wrap:  true,
					Spacing: "medium",
				},
			},
		})
	} else {
		// Generic message card with nice styling
		card.Body = append(card.Body, CardElement{
			Type: "Container",
			Items: []CardElement{
				{
					Type:  "TextBlock",
					Text:  message,
					Wrap:  true,
					Size:  "medium",
				},
			},
		})
	}

	return card
}

// TeamsClaims represents the claims in a Microsoft Teams JWT token
type TeamsClaims struct {
	jwt.RegisteredClaims
	AppID string `json:"appid"`
	// Azure AD v2 token claims
	Aud string `json:"aud"` // Audience - should be our app ID
	Iss string `json:"iss"` // Issuer - should be login.microsoftonline.com
	Scp string `json:"scp"` // Scopes
}

// HomePage serves the main landing page
func (h *Handler) HomePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`<html><body><h1>Jeembot</h1><p>ClickUp integration for Microsoft Teams</p></body></html>`))
}

// PrivacyPage serves the privacy policy page
func (h *Handler) PrivacyPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`<html><body><h1>Privacy Policy</h1><p>Jeembot stores only task data necessary for ClickUp integration.</p></body></html>`))
}

// TermsOfServicePage serves the terms of service page
func (h *Handler) TermsOfServicePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`<html><body><h1>Terms of Service</h1><p>By using Jeembot, you agree to use it in accordance with applicable laws.</p></body></html>`))
}

// validateJWT validates the JWT token from the Authorization header
// For Microsoft Teams Bot Framework, tokens are Azure AD v2 access tokens
//
// NOTE: This implements basic JWT structure validation. For production,
// you should validate the signature against Microsoft's JWKS endpoint:
// https://login.microsoftonline.com/{tenant}/v2.0/.well-known/openid-configuration
//
// The Bot Framework also provides a token validation endpoint:
// POST https://api.botframework.com/.auth/v2/validateToken
func (h *Handler) validateJWT(authHeader string) bool {
	if authHeader == "" {
		log.Println("[WARN] Missing Authorization header")
		return false
	}

	// Extract token from "Bearer <token>" format
	var tokenString string
	authHeaderUpper := strings.ToUpper(authHeader)
	if strings.HasPrefix(authHeaderUpper, "BEARER ") {
		tokenString = strings.TrimPrefix(authHeader, "Bearer ")
	} else {
		tokenString = authHeader
	}

	if tokenString == "" {
		log.Println("[WARN] Empty JWT token")
		return false
	}

	// Parse token without signature validation (for development)
	// In production, implement JWKS validation
	token, _, err := jwt.NewParser().ParseUnverified(tokenString, &TeamsClaims{})
	if err != nil {
		log.Printf("[WARN] JWT parsing error: %v", err)
		return false
	}

	claims, ok := token.Claims.(*TeamsClaims)
	if !ok {
		log.Println("[WARN] JWT claims not as expected")
		return false
	}

	// Validate audience matches our app ID
	if claims.Aud != h.config.TeamsAppID {
		log.Printf("[WARN] JWT audience mismatch. Expected: %s, Got: %s", h.config.TeamsAppID, claims.Aud)
		return false
	}

	// Validate issuer is from Microsoft
	if !strings.Contains(claims.Iss, "https://api.botframework.com") {
		log.Printf("[WARN] JWT issuer not from Microsoft. Got: %s", claims.Iss)
		return false
	}

	log.Printf("[DEBUG] JWT validation passed for audience: %s", claims.Aud)
	return true
}
