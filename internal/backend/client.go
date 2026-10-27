package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Task represents a claimed task from the backend.
//
// FilePaths is the claim-time file selection from POST /api/tasks:
// empty means the full torrent; non-empty MUST limit which torrent files
// this worker downloads and uploads (tolerant path match — see media.MatchSelected).
type Task struct {
	TaskID      string       `json:"task_id"`
	Magnet      string       `json:"magnet"`
	MediaTitle  string       `json:"media_title"`
	FilePaths   []string     `json:"file_paths"`
	UploadSlots []UploadSlot `json:"upload_slots"`
}

type UploadSlot struct {
	Path    string            `json:"path"`
	S3Key   string            `json:"s3_key"`
	PutURL  string            `json:"put_url"`
	Headers map[string]string `json:"headers"`
}

type FileInfo struct {
	Path      string `json:"path"`
	S3Key     string `json:"s3_key,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
}

// ManifestFile is one torrent file reported to backend after metadata.
type ManifestFile struct {
	Path       string   `json:"path"`
	SizeBytes  int64    `json:"size_bytes"`
	Kind       string   `json:"kind,omitempty"` // video|audio|subtitle|other
	Season     int      `json:"season,omitempty"`
	Episode    int      `json:"episode,omitempty"`
	EpisodeEnd int      `json:"episode_end,omitempty"`
	Language   string   `json:"language,omitempty"`
	Flags      []string `json:"flags,omitempty"`
	Primary    bool     `json:"primary,omitempty"`
}

// StreamProgress reports progressive download / seek window for playback.
type StreamProgress struct {
	FilePath          string  `json:"file_path"`
	SizeBytes         int64   `json:"size_bytes"`
	ContiguousBytes   int64   `json:"contiguous_bytes"` // max seekable offset (exclusive)
	DownloadedBytes   int64   `json:"downloaded_bytes"`
	PercentDownloaded float64 `json:"percent_downloaded"`
	PercentSeekable   float64 `json:"percent_seekable"`
	SegmentSize       int64   `json:"segment_size"`
	SegmentsTotal     int     `json:"segments_total"`
	SegmentsUploaded  int     `json:"segments_uploaded"`
	SegmentsAvailable int     `json:"segments_available"`
	StreamReady       bool    `json:"stream_ready"` // enough prefix to start playback
	Complete          bool    `json:"complete"`
}

type HeartbeatTask struct {
	TaskID      string  `json:"task_id"`
	Stage       string  `json:"stage"`
	ProgressPct float64 `json:"progress_pct"`
	SpeedBps    int64   `json:"speed_bps"`
	// Stream is optional; present while downloading a streamable file.
	Stream *StreamProgress `json:"stream,omitempty"`
}

// Client talks to the Seans backend API.
type Client struct {
	baseURL    string
	regSecret  string
	httpClient *http.Client
	logger     *slog.Logger

	mu           sync.Mutex
	workerToken  string
	reRegistered bool
}

func New(baseURL, regSecret string, logger *slog.Logger) *Client {
	return &Client{
		baseURL:   baseURL,
		regSecret: regSecret,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		logger: logger,
	}
}

// IsRegistered returns true if the client has a valid worker token.
func (c *Client) IsRegistered() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.workerToken != ""
}

func (c *Client) workerTokenLocked() string {
	return c.workerToken
}

// Register registers the worker with the backend. Called once at startup.
func (c *Client) Register(ctx context.Context, name string) error {
	body := map[string]any{
		"name":       name,
		"reg_secret": c.regSecret,
	}

	var resp map[string]any
	if err := c.doJSON(ctx, http.MethodPost, "/api/worker/register", "", body, &resp); err != nil {
		return fmt.Errorf("register: %w", err)
	}

	token, ok := resp["worker_token"].(string)
	if !ok || token == "" {
		return fmt.Errorf("register: no worker_token in response")
	}

	c.mu.Lock()
	c.workerToken = token
	c.reRegistered = false
	c.mu.Unlock()

	c.logger.Info("registered with backend", "token_prefix", token[:8]+"...")
	return nil
}

// Claim requests the next task from the backend. Returns nil if no task available.
func (c *Client) Claim(ctx context.Context) (*Task, error) {
	c.mu.Lock()
	token := c.workerToken
	c.mu.Unlock()

	var resp map[string]any
	if err := c.doJSON(ctx, http.MethodPost, "/api/worker/claim", token, nil, &resp); err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}

	// Backend returns null when no task available
	if resp == nil {
		return nil, nil
	}

	taskID, _ := resp["task_id"].(string)
	magnet, _ := resp["magnet"].(string)

	media, _ := resp["media"].(map[string]any)
	mediaTitle := ""
	if media != nil {
		mediaTitle, _ = media["title"].(string)
	}

	filePathsRaw, _ := resp["file_paths"].([]any)
	filePaths := make([]string, 0, len(filePathsRaw))
	for _, fp := range filePathsRaw {
		if s, ok := fp.(string); ok {
			filePaths = append(filePaths, s)
		}
	}

	return &Task{
		TaskID:     taskID,
		Magnet:     magnet,
		MediaTitle: mediaTitle,
		FilePaths:  filePaths,
	}, nil
}

// Manifest sends discovered file list to the backend after torrent metadata is available.
func (c *Client) Manifest(ctx context.Context, taskID string, files []ManifestFile, catalog map[string]any) ([]UploadSlot, error) {
	body := map[string]any{
		"task_id": taskID,
		"files":   files,
	}
	if catalog != nil {
		body["catalog"] = catalog
	}

	var resp map[string]any
	if err := c.doAuthJSON(ctx, http.MethodPost, "/api/worker/manifest", body, &resp); err != nil {
		return nil, fmt.Errorf("manifest %s: %w", taskID, err)
	}

	slotsRaw, _ := resp["upload_slots"].([]any)
	slots := make([]UploadSlot, 0, len(slotsRaw))
	for _, s := range slotsRaw {
		sm, ok := s.(map[string]any)
		if !ok {
			continue
		}
		slot := UploadSlot{
			Path:   fmt.Sprintf("%v", sm["path"]),
			S3Key:  fmt.Sprintf("%v", sm["s3_key"]),
			PutURL: fmt.Sprintf("%v", sm["put_url"]),
		}
		if hdrs, ok := sm["headers"].(map[string]any); ok {
			slot.Headers = make(map[string]string)
			for k, v := range hdrs {
				slot.Headers[k] = fmt.Sprintf("%v", v)
			}
		}
		slots = append(slots, slot)
	}

	return slots, nil
}

// Complete reports task success.
func (c *Client) Complete(ctx context.Context, taskID string, files []FileInfo) error {
	body := map[string]any{
		"task_id": taskID,
		"files":   files,
	}
	if err := c.doAuthJSON(ctx, http.MethodPost, "/api/worker/complete", body, nil); err != nil {
		return fmt.Errorf("complete %s: %w", taskID, err)
	}
	return nil
}

// Fail reports task failure.
func (c *Client) Fail(ctx context.Context, taskID, reason string) error {
	body := map[string]any{
		"task_id": taskID,
		"error":   reason,
	}
	if err := c.doAuthJSON(ctx, http.MethodPost, "/api/worker/fail", body, nil); err != nil {
		return fmt.Errorf("fail %s: %w", taskID, err)
	}
	return nil
}

// Heartbeat sends progress for a single task.
func (c *Client) Heartbeat(ctx context.Context, task HeartbeatTask) error {
	body := map[string]any{
		"task_id":      task.TaskID,
		"stage":        task.Stage,
		"progress_pct": task.ProgressPct,
		"speed_bps":    task.SpeedBps,
	}
	if task.Stream != nil {
		body["stream"] = task.Stream
	}
	if err := c.doAuthJSON(ctx, http.MethodPost, "/api/worker/heartbeat", body, nil); err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	return nil
}

func (c *Client) doJSON(ctx context.Context, method, path, token string, body any, result any) error {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Worker-Token", token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 204 {
		return nil
	}

	if resp.StatusCode == 401 {
		c.mu.Lock()
		if c.reRegistered {
			c.mu.Unlock()
			return fmt.Errorf("401 after re-register, backing off")
		}
		c.logger.Warn("received 401, clearing token for re-registration")
		c.workerToken = ""
		c.reRegistered = true
		c.mu.Unlock()
		return fmt.Errorf("401: need re-registration")
	}
	if resp.StatusCode >= 500 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("server error %d: %s", resp.StatusCode, string(respBody))
	}
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("client error %d: %s", resp.StatusCode, string(respBody))
	}

	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

func (c *Client) doAuthJSON(ctx context.Context, method, path string, body any, result any) error {
	c.mu.Lock()
	token := c.workerToken
	c.mu.Unlock()
	if token == "" {
		return fmt.Errorf("not registered")
	}
	return c.doJSON(ctx, method, path, token, body, result)
}

// ClearReRegistrationFlag resets the 401 re-registration flag.
func (c *Client) ClearReRegistrationFlag() {
	c.mu.Lock()
	c.reRegistered = false
	c.mu.Unlock()
}
