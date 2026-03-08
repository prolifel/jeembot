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
		h.sendResponse(w, "Invalid list. Use: cti, cloudsec, or soc\nExample: jmbot /to cti Your task description")
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
		log.Printf("[DEBUG] InstallationUpdate activity received, action: %s", activity.Action)

		if activity.Action == "add" {
			// Bot was installed - send welcome message
			h.handleBotAdded(w, &activity)
			return
		} else if activity.Action == "remove" {
			// Bot was uninstalled - send farewell message if possible
			scope := getConversationScope(&activity)
			log.Printf("[INFO] Bot removed from conversation, scope: %s", scope)

			// Try to send farewell message
			farewellMessage := "jmbot has been removed from this conversation. \n\nIf you need to reinstall, just search for jmbot in the Teams app store. \n\nThanks for using jmbot! 👋"

			if scope == "personal" {
				// For personal scope, send farewell directly
				h.sendBotResponse(w, farewellMessage, &activity)
				return
			} else {
				// For team/groupChat, try to send 1:1 to installer
				installerID := ""
				if activity.From != nil {
					installerID = activity.From.ID
				}
				if installerID != "" {
					// Try to send 1:1 farewell message
					h.sendProactiveFarewellToUser(w, &activity, installerID)
					return
				}
			}
		}
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
// Handles both conversationUpdate (membersAdded) and installationUpdate (action=add)
func (h *Handler) handleBotAdded(w http.ResponseWriter, activity *Activity) bool {
	// Check if this is an installationUpdate with action=add
	isInstallationUpdate := activity.Type == "installationUpdate" && activity.Action == "add"

	// Check if membersAdded contains our bot (for conversationUpdate)
	botAdded := false
	if activity.MembersAdded != nil {
		botID := h.config.TeamsAppID
		for _, member := range activity.MembersAdded {
			// Member ID may have prefix like "28:" - strip it for comparison
			memberID := member.ID
			if idx := strings.LastIndex(memberID, ":"); idx >= 0 {
				memberID = memberID[idx+1:]
			}
			if memberID == botID {
				botAdded = true
				break
			}
		}
	}

	// Process if bot was added via either method
	if botAdded || isInstallationUpdate {
		// Bot was added - determine conversation scope
		scope := getConversationScope(activity)
		log.Printf("[INFO] Bot added to conversation, scope: %s, source: %s", scope, activity.Type)

		// Get installer info from activity.From
		installerID := ""
		installerName := "there"
		if activity.From != nil {
			installerID = activity.From.ID
			installerName = activity.From.Name
			if installerName == "" {
				installerName = "there"
			}
		}

		if scope == "personal" {
			// Personal scope - for conversationUpdate, conversation exists, send directly
			// For installationUpdate, we need to create a new conversation
			if isInstallationUpdate && installerID != "" {
				// Create new 1:1 conversation for installationUpdate
				tenantID := ""
				if activity.Conversation != nil {
					tenantID = activity.Conversation.TenantID
				}
				conversationID, err := h.createConversation(installerID, activity.ServiceURL, tenantID)
				if err != nil {
					log.Printf("[ERROR] Failed to create 1:1 conversation for personal scope: %v", err)
					w.WriteHeader(http.StatusOK)
					return true
				}
				// Send welcome message to the new conversation
				welcomeMsg := "Welcome to jmbot! 🎉\n\nI help you create tasks in ClickUp directly from Microsoft Teams.\n\nAvailable commands:\n\n• /to cti <task> - Create task in CTI list\n• /to cloudsec <task> - Create task in CloudSec list\n• /to soc <task> - Create task in SOC list\n\nExample: /to cti Fix login bug\n\nJust type your task and I'll create it for you!"
				resp := Activity{
					Type:         "message",
					TextFormat:   "plain",
					From:         activity.Recipient,
					Conversation: &ConversationAccount{ID: conversationID},
					ChannelID:    activity.ChannelID,
					ServiceURL:   activity.ServiceURL,
					Text:         welcomeMsg,
				}
				if err := h.sendToTeams(&resp); err != nil {
					log.Printf("[ERROR] Failed to send welcome to new personal conversation: %v", err)
				}
			} else {
				// For conversationUpdate, conversation exists - send directly
				h.sendBotResponse(w, "Welcome to jmbot! 🎉\n\nI help you create tasks in ClickUp directly from Microsoft Teams.\n\nAvailable commands:\n\n• /to cti <task> - Create task in CTI list\n• /to cloudsec <task> - Create task in CloudSec list\n• /to soc <task> - Create task in SOC list\n\nExample: /to cti Fix login bug\n\nJust type your task and I'll create it for you!", activity)
			}
		} else {
			// Team or GroupChat scope - send welcome to the team channel directly
			welcomeMsg := fmt.Sprintf("Hi %s! 👋\n\nThanks for adding jmbot to this team! I'm ready to help you create tasks in ClickUp directly from Teams.\n\n**Available commands:**\n• `/to cti <task>` - Create task in CTI list\n• `/to cloudsec <task>` - Create task in CloudSec list\n• `/to soc <task>` - Create task in SOC list\n\n**Example:** `/to cti Fix login bug`\n\nJust type your task and I'll create it for you!", installerName)

			resp := Activity{
				Type:         "message",
				TextFormat:   "plain",
				From:         activity.Recipient,
				Conversation: &ConversationAccount{ID: activity.Conversation.ID},
				ChannelID:    activity.ChannelID,
				ServiceURL:   activity.ServiceURL,
				Text:         welcomeMsg,
			}
			if err := h.sendToTeams(&resp); err != nil {
				log.Printf("[ERROR] Failed to send welcome to team channel: %v", err)
			}
		}
		return true
	}
	return false
}

// getConversationScope returns the scope of the conversation: "personal", "team", or "groupChat"
func getConversationScope(activity *Activity) string {
	// First check ConversationType from the conversation account
	if activity.Conversation != nil && activity.Conversation.ConversationType != "" {
		convType := activity.Conversation.ConversationType
		// Map Teams-specific conversation types
		if convType == "channel" {
			return "team"
		}
		return convType
	}

	// Then check Teams-specific channel data
	if activity.ChannelData != nil {
		// Try to marshal and unmarshal as TeamsChannelData
		data, err := json.Marshal(activity.ChannelData)
		if err == nil {
			var teamsData TeamsChannelData
			if err := json.Unmarshal(data, &teamsData); err == nil {
				// If we have team info, it's a team scope
				if teamsData.Team != nil {
					return "team"
				}
				// Check chatType
				if teamsData.ChatType != "" {
					chatType := teamsData.ChatType
					if chatType == "channel" {
						return "team"
					}
					return chatType
				}
			}
		}
	}

	// Default to personal if we can't determine
	return "personal"
}

// sendProactiveWelcomeToUser sends a 1:1 welcome message to a user
func (h *Handler) sendProactiveWelcomeToUser(w http.ResponseWriter, activity *Activity, installerID, installerName, scope string) {
	// Get tenant ID from conversation
	tenantID := ""
	if activity.Conversation != nil {
		tenantID = activity.Conversation.TenantID
	}

	// Create a new 1:1 conversation with the installer using the service URL from the activity
	conversationID, err := h.createConversation(installerID, activity.ServiceURL, tenantID)
	if err != nil {
		log.Printf("[ERROR] Failed to create 1:1 conversation: %v", err)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Build welcome message based on scope
	var welcomeMessage string
	if scope == "team" {
		welcomeMessage = fmt.Sprintf("Hi %s! 👋\n\nThanks for adding jmbot to your team! I've been installed by %s and I'm ready to help you create tasks in ClickUp directly from Teams.\n\n**What I can do:**\n• Create tasks in CTI, CloudSec, or SOC lists\n• Help you track work without leaving Teams\n\n**Available commands:**\n• `/to cti <task>` - Create task in CTI list\n• `/to cloudsec <task>` - Create task in CloudSec list\n• `/to soc <task>` - Create task in SOC list\n\n**Example:** `/to cti Fix login bug`\n\nJust type your task and I'll create it for you in ClickUp!", installerName, installerName)
	} else {
		welcomeMessage = fmt.Sprintf("Hi %s! 👋\n\nThanks for adding jmbot to this group chat! I'm ready to help you create tasks in ClickUp directly from Teams.\n\n**Available commands:**\n• `/to cti <task>` - Create task in CTI list\n• `/to cloudsec <task>` - Create task in CloudSec list\n• `/to soc <task>` - Create task in SOC list\n\n**Example:** `/to cti Review security alert`\n\nJust type your task and I'll create it for you!", installerName)
	}

	// Send the welcome message to the new conversation
	resp := Activity{
		Type:         "message",
		TextFormat:   "plain",
		From:         activity.Recipient,
		Conversation: &ConversationAccount{ID: conversationID},
		ChannelID:    activity.ChannelID,
		ServiceURL:   activity.ServiceURL,
		Text:         welcomeMessage,
	}

	if err := h.sendToTeams(&resp); err != nil {
		log.Printf("[ERROR] Failed to send proactive welcome: %v", err)
	}

	w.WriteHeader(http.StatusOK)
}

// createConversation creates a new 1:1 conversation with a user
func (h *Handler) createConversation(userID, serviceURL, tenantID string) (string, error) {
	if serviceURL == "" {
		return "", fmt.Errorf("no service URL provided")
	}

	// Get OAuth token
	token, err := h.getBotToken()
	if err != nil {
		return "", fmt.Errorf("failed to get bot token: %w", err)
	}

	// Build the Bot Framework API URL for creating conversations
	apiURL := strings.TrimRight(serviceURL, "/")

	// Create conversation request with tenant data
	convReq := struct {
		Bot         *ChannelAccount        `json:"bot"`
		Members     []ChannelAccount       `json:"members"`
		ChannelData map[string]interface{} `json:"channelData,omitempty"`
	}{
		Bot: &ChannelAccount{
			ID: h.config.TeamsAppID,
		},
		Members: []ChannelAccount{
			{ID: userID},
		},
	}

	// Add tenant data if available
	if tenantID != "" {
		convReq.ChannelData = map[string]interface{}{
			"tenant": map[string]string{"id": tenantID},
		}
	}

	body, err := json.Marshal(convReq)
	if err != nil {
		return "", fmt.Errorf("failed to marshal conversation request: %w", err)
	}

	url := fmt.Sprintf("%s/v3/conversations", apiURL)
	log.Printf("[DEBUG] Creating conversation with: %s", url)

	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("Teams API returned status %d: %s", resp.StatusCode, string(respBody))
	}

	// Parse response to get conversation ID
	var convResp struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&convResp); err != nil {
		return "", fmt.Errorf("failed to parse conversation response: %w", err)
	}

	log.Printf("[DEBUG] Created conversation: %s", convResp.ID)
	return convResp.ID, nil
}

// sendProactiveFarewellToUser sends a 1:1 farewell message when bot is removed
func (h *Handler) sendProactiveFarewellToUser(w http.ResponseWriter, activity *Activity, installerID string) {
	farewellMessage := "jmbot has been removed from a team or group chat. \n\nIf you need to reinstall, just search for jmbot in the Teams app store. \n\nThanks for using jmbot! 👋"

	// Get tenant ID from conversation
	tenantID := ""
	if activity.Conversation != nil {
		tenantID = activity.Conversation.TenantID
	}

	// Create a new 1:1 conversation with the installer
	conversationID, err := h.createConversation(installerID, activity.ServiceURL, tenantID)
	if err != nil {
		log.Printf("[ERROR] Failed to create 1:1 conversation for farewell: %v", err)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Send the farewell message to the new conversation
	resp := Activity{
		Type:         "message",
		TextFormat:   "plain",
		From:         activity.Recipient,
		Conversation: &ConversationAccount{ID: conversationID},
		ChannelID:    activity.ChannelID,
		ServiceURL:   activity.ServiceURL,
		Text:         farewellMessage,
	}

	if err := h.sendToTeams(&resp); err != nil {
		log.Printf("[ERROR] Failed to send farewell message: %v", err)
	}

	w.WriteHeader(http.StatusOK)
}

// stripMentionTags removes Teams mention tags like <at>jmbot</at> from message text
func stripMentionTags(text string) string {
	// Remove <at>...</at> pattern used by Teams for bot mentions
	re := regexp.MustCompile(`<at[^>]*>.*?</at>`)
	return re.ReplaceAllString(text, "")
}

// handleBotMessage processes a message activity from Teams
func (h *Handler) handleBotMessage(w http.ResponseWriter, activity *Activity) {
	// Strip Teams mention tags (e.g., <at>jmbot</at>) from message text
	cleanText := stripMentionTags(activity.Text)

	// Check for greeting commands first
	text := strings.TrimSpace(strings.ToLower(cleanText))
	if text == "hi" || text == "hello" || text == "hello!" {
		h.sendBotResponse(w, "Hello! I'm jmbot!\n\nI help you create tasks in ClickUp without leaving Teams.\n\nUse /to <list> <task> to create tasks:\n\n• /to cti <task> - CTI team\n\n• /to cloudsec <task> - CloudSec team\n\n• /to soc <task> - SOC team\n\nExample: /to cti Review security alert", activity)
		return
	}
	if text == "help" {
		h.sendBotResponse(w, "jmbot Help\n\nCreate tasks in ClickUp using:\n\n/to <team> <task description>\n\nTeams:\n\n• cti - CTI team\n\n• cloudsec - CloudSec team\n\n• soc - SOC team\n\nExamples:\n\n• /to cti Update firewall rules\n\n• /to cloudsec Review access request\n\n• /to soc Investigate alert #123\n\nNeed help? Just ask!", activity)
		return
	}
	// Handle "Create Task" command from manifest commandList
	if text == "create task" || text == "create task!" {
		h.sendBotResponse(w, "📝 Create a New Task in ClickUp\n\nI can create tasks in three different team lists:\n\n**Available Lists:**\n\n• **CTI** - CTI team tasks\n\n• **CloudSec** - CloudSec team tasks\n\n• **SOC** - SOC team tasks\n\n**How to Use:**\n\n`/to <list> <your task>`\n\n**Examples:**\n\n• `/to cti Review security alert`\n\n• `/to cloudsec Update firewall rules`\n\n• `/to soc Investigate alert #123`\n\nJust type your task and I'll create it for you!", activity)
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
// Includes retry logic for transient network failures
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

	// Encode the activity once (for retries)
	body, err := json.Marshal(activity)
	if err != nil {
		return fmt.Errorf("failed to marshal activity: %w", err)
	}

	// Retry up to 2 times with exponential backoff for transient errors
	maxRetries := 2
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff: 1s, 2s
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			log.Printf("[DEBUG] Retry attempt %d after %v", attempt, backoff)
			time.Sleep(backoff)
		}

		// Create HTTP client with timeout
		client := &http.Client{Timeout: 30 * time.Second}

		// Create request
		req, err := http.NewRequest("POST", url, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("failed to send request: %w", err)
			log.Printf("[WARN] Attempt %d failed: %v", attempt+1, err)
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 400 {
			respBody, _ := io.ReadAll(resp.Body)
			lastErr = fmt.Errorf("Teams API returned status %d: %s", resp.StatusCode, string(respBody))
			log.Printf("[WARN] Attempt %d failed: %v", attempt+1, lastErr)
			continue
		}

		log.Printf("[DEBUG] Response sent to Teams successfully")
		return nil
	}

	return fmt.Errorf("failed after %d attempts: %w", maxRetries+1, lastErr)
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

	client := &http.Client{Timeout: 30 * time.Second}
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
// Handles Teams webhook HTML format: "<p><at>jmbot</at>&nbsp;/to cloudsec hello world</p>"
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
// Expected format: jmbot /to <list> <task detail>
// Handles HTML format: "<p><at>jmbot</at>&nbsp;/to cloudsec hello world</p>"
func parseCommand(text string) (listName, taskDetail string, err error) {
	// Clean HTML: strip tags and decode entities
	text = cleanHTML(text)

	log.Printf("[DEBUG] Text: %s", text)

	// Find jmbot mention (case-insensitive)
	text = strings.ToLower(text)
	mentionIdx := strings.Index(text, "jmbot")
	if mentionIdx == -1 {
		return "", "", fmt.Errorf("Invalid syntax. Use: jmbot /to <cti|cloudsec|soc> <task detail>")
	}

	// Extract text after mention
	afterMention := strings.TrimSpace(text[mentionIdx+len("jmbot"):])

	// Split by whitespace
	parts := strings.Fields(afterMention)
	if len(parts) < 2 {
		return "", "", fmt.Errorf("Invalid syntax. Use: jmbot /to <cti|cloudsec|soc> <task detail>")
	}

	// Check for /to command
	if parts[0] != "/to" {
		return "", "", fmt.Errorf("Invalid syntax. Use: jmbot /to <cti|cloudsec|soc> <task detail>")
	}

	// Validate list name
	listName = parts[1]
	if listName != "cti" && listName != "cloudsec" && listName != "soc" {
		return "", "", fmt.Errorf("Invalid list. Use: cti, cloudsec, or soc\nExample: jmbot /to cti Your task description")
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
		Type: "message",
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

		// Build well-formatted card with extended description
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
									Type:                "TextBlock",
									Text:                "Task Created Successfully!",
									Weight:              "bolder",
									Size:                "medium",
									Color:               "good",
									HorizontalAlignment: "left",
								},
								{
									Type:                "TextBlock",
									Text:                "Your task has been created and saved in ClickUp",
									IsSubtle:            true,
									HorizontalAlignment: "left",
									Size:                "small",
								},
							},
						},
					},
				},
			},
		})

		// Add task details in a styled container
		card.Body = append(card.Body, CardElement{
			Type:  "Container",
			Style: "emphasis",
			Items: []CardElement{
				{
					Type:   "TextBlock",
					Text:   "📝 " + taskName,
					Wrap:   true,
					Weight: "bolder",
					Size:   "medium",
				},
				{
					Type: "FactSet",
					Facts: []Fact{
						{Title: "List:", Value: listName},
						{Title: "Status:", Value: "Open"},
						{Title: "Priority:", Value: "Normal"},
					},
				},
			},
		})

		// Add helpful next steps text
		card.Body = append(card.Body, CardElement{
			Type: "Container",
			Items: []CardElement{
				{
					Type:     "TextBlock",
					Text:     "You can now assign this task, add due dates, or update priorities directly in ClickUp.",
					Wrap:     true,
					IsSubtle: true,
					Size:     "small",
					Spacing:  "medium",
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
				{
					Type:  "Action.OpenUrl",
					Title: "📋 View All Tasks",
					URL:   "https://app.clickup.com",
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
									Type:                "TextBlock",
									Text:                "Welcome to jmbot",
									Weight:              "bolder",
									Size:                "large",
									HorizontalAlignment: "left",
								},
							},
						},
					},
				},
				{
					Type:    "TextBlock",
					Text:    message,
					Wrap:    true,
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
					Type: "TextBlock",
					Text: message,
					Wrap: true,
					Size: "medium",
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
	w.Write([]byte(`<html><body><h1>jmbot</h1><p>ClickUp integration for Microsoft Teams</p></body></html>`))
}

// PrivacyPage serves the privacy policy page
func (h *Handler) PrivacyPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`<html><body><h1>Privacy Policy</h1><p>jmbot stores only task data necessary for ClickUp integration.</p></body></html>`))
}

// TermsOfServicePage serves the terms of service page
func (h *Handler) TermsOfServicePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`<html><body><h1>Terms of Service</h1><p>By using jmbot, you agree to use it in accordance with applicable laws.</p></body></html>`))
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
