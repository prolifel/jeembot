package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// ClickUpService handles communication with ClickUp API
type ClickUpService struct {
	client    *http.Client
	baseURL   string
	token     string
	listIDs   map[string]string
	assignees map[string]string
}

// NewClickUpService creates a new ClickUp service instance
func NewClickUpService(config *Config) *ClickUpService {
	return &ClickUpService{
		client: &http.Client{
			Timeout: 3 * time.Second,
		},
		baseURL: "https://api.clickup.com/api/v2",
		token:   config.ClickUpAPIToken,
		listIDs: map[string]string{
			"cti":      config.ClickUpListCTI,
			"cloudsec": config.ClickUpListCloudSec,
			"soc":      config.ClickUpListSOC,
		},
		assignees: map[string]string{
			"cti":      config.ClickUpAssigneeCTI,
			"cloudsec": config.ClickUpAssigneeCloudSec,
			"soc":      config.ClickUpAssigneeSOC,
		},
	}
}

// GetListID returns the ClickUp list ID for the given list name
func (s *ClickUpService) GetListID(listName string) (string, bool) {
	id, ok := s.listIDs[listName]
	return id, ok
}

// CreateTask creates a new task in ClickUp
func (s *ClickUpService) CreateTask(listName, listID, name, description string) (*ClickUpTaskResponse, error) {
	log.Printf("[DEBUG] CreateTask - List: %s, ListID: %s, Name: %s", listName, listID, name)

	// Get assignee for this list type
	var assignees []string
	if assignee, ok := s.assignees[listName]; ok && assignee != "" {
		assignees = []string{assignee}
		log.Printf("[DEBUG] CreateTask - Assignee: %s", assignee)
	}

	taskReq := ClickUpTaskRequest{
		Name:        name,
		Description: description,
		Assignees:   assignees,
	}

	body, err := json.Marshal(taskReq)
	if err != nil {
		log.Printf("[ERROR] CreateTask - Failed to marshal request: %v", err)
		return nil, fmt.Errorf("failed to marshal task request: %w", err)
	}

	log.Printf("[DEBUG] CreateTask - Request body: %s", string(body))

	url := fmt.Sprintf("%s/list/%s/task", s.baseURL, listID)
	log.Printf("[DEBUG] CreateTask - URL: %s", url)

	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		log.Printf("[ERROR] CreateTask - Failed to create request: %v", err)
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", s.token)
	req.Header.Set("Content-Type", "application/json")

	log.Printf("[DEBUG] CreateTask - Sending request to ClickUp...")
	resp, err := s.client.Do(req)
	if err != nil {
		log.Printf("[ERROR] CreateTask - Failed to send request: %v", err)
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	log.Printf("[DEBUG] CreateTask - Response status: %d", resp.StatusCode)

	if resp.StatusCode != http.StatusOK {
		respStr, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("[ERROR] CreateTask - Failed to readall")
			return nil, fmt.Errorf("clickup API error: failed to readall")
		}

		var errResp ClickUpErrorResponse
		if json.NewDecoder(resp.Body).Decode(&errResp) == nil {
			log.Printf("[ERROR] CreateTask - ClickUp API error: %s (code: %d)", errResp.Err, errResp.ECode)
			return nil, fmt.Errorf("clickup API error: %s (code: %d)", errResp.Err, errResp.ECode)
		}

		log.Printf("[ERROR] CreateTask - ClickUp API returned status %d, %s", resp.StatusCode, string(respStr))
		return nil, fmt.Errorf("clickup API returned status %d", resp.StatusCode)
	}

	var taskResp ClickUpTaskResponse
	if err := json.NewDecoder(resp.Body).Decode(&taskResp); err != nil {
		log.Printf("[ERROR] CreateTask - Failed to decode response: %v", err)
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	log.Printf("[DEBUG] CreateTask - Task created successfully: ID=%s, Name=%s", taskResp.ID, taskResp.Name)

	// Construct ClickUp task URL
	taskResp.URL = fmt.Sprintf("https://app.clickup.com/t/%s", taskResp.ID)

	return &taskResp, nil
}
