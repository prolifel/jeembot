package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"unicode/utf8"
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

// sendResponse sends a Teams-compatible JSON response
func (h *Handler) sendResponse(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	resp := TeamsResponse{
		Type: "message",
		Text: message,
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("Error encoding response: %v", err)
	}
}
