package s3uploader

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Uploader handles S3 uploads via rclone.
type Uploader struct {
	configPath string
	bucket     string
	mockS3     bool
	logger     *slog.Logger
}

// New creates an Uploader and generates the rclone config file.
func New(stateDir, s3AccessKey, s3SecretKey, s3Endpoint, s3Bucket, s3Region string, mockS3 bool, logger *slog.Logger) (*Uploader, error) {
	u := &Uploader{
		bucket: s3Bucket,
		mockS3: mockS3,
		logger: logger,
	}

	configPath := filepath.Join(stateDir, "rclone.conf")
	u.configPath = configPath

	if err := u.generateConfig(stateDir, s3AccessKey, s3SecretKey, s3Endpoint, s3Region); err != nil {
		return nil, fmt.Errorf("generate rclone config: %w", err)
	}

	return u, nil
}

func (u *Uploader) generateConfig(stateDir, s3AccessKey, s3SecretKey, s3Endpoint, s3Region string) error {
	var config string
	if u.mockS3 {
		mockDir := "/tmp/seans-s3-mock"
		os.MkdirAll(mockDir, 0o755)
		config = `[seans]
type = local
`
	} else {
		config = fmt.Sprintf(`[seans]
type = s3
provider = Other
access_key_id = %s
secret_access_key = %s
endpoint = %s
region = %s
force_path_style = true
acl = private
`, s3AccessKey, s3SecretKey, s3Endpoint, s3Region)
	}

	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("mkdir state dir: %w", err)
	}
	return os.WriteFile(u.configPath, []byte(config), 0o600)
}

// Move uploads files from localPath to the S3 prefix using rclone move.
// Retries up to maxAttempts times.
func (u *Uploader) Move(ctx context.Context, localPath, destPrefix string, maxAttempts int, logFile string) error {
	var remotePath string
	if u.mockS3 {
		remotePath = filepath.Join("/tmp/seans-s3-mock", destPrefix)
		os.MkdirAll(remotePath, 0o755)
	} else {
		remotePath = "seans:" + u.bucket + "/" + destPrefix
	}

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err := u.runRclone(ctx, localPath, remotePath, logFile)
		if err == nil {
			return nil
		}
		u.logger.Warn("rclone attempt failed", "attempt", attempt, "max", maxAttempts, "error", err)
		if attempt < maxAttempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 5 * time.Second):
			}
		}
	}
	return fmt.Errorf("rclone failed after %d attempts", maxAttempts)
}

func (u *Uploader) runRclone(ctx context.Context, localPath, remotePath, logFile string) error {
	args := []string{
		"move",
		localPath,
		remotePath,
		"--transfers", "4",
		"--checkers", "8",
		"--s3-chunk-size", "64M",
		"--s3-upload-concurrency", "4",
		"--s3-acl", "private",
		"--fast-list",
		"-v",
		"--stats", "15s",
	}
	if logFile != "" {
		args = append(args, "--log-file", logFile)
	}

	cmd := exec.CommandContext(ctx, "rclone", args...)
	cmd.Env = append(os.Environ(), "RCLONE_CONFIG="+u.configPath)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("rclone stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("rclone stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("rclone start: %w", err)
	}

	// Log rclone output
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			u.logger.Debug("rclone", "out", scanner.Text())
		}
	}()
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "ERROR") || strings.Contains(line, "error") {
				u.logger.Warn("rclone", "err", line)
			} else {
				u.logger.Debug("rclone", "err", line)
			}
		}
	}()

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("rclone exit: %w", err)
	}
	return nil
}

// remotePath builds the rclone remote path for destPrefix (S3 key prefix).
func (u *Uploader) remotePath(destPrefix string) string {
	if u.mockS3 {
		p := filepath.Join("/tmp/seans-s3-mock", destPrefix)
		os.MkdirAll(filepath.Dir(p), 0o755)
		return p
	}
	return "seans:" + u.bucket + "/" + destPrefix
}

// UploadFile uploads a single local file to destKey (object key / path suffix).
// destKey should use forward slashes, e.g. "users/task/stream/seg_000001".
func (u *Uploader) UploadFile(ctx context.Context, localPath, destKey string, logFile string) error {
	remote := u.remotePath(destKey)
	if err := os.MkdirAll(filepath.Dir(remote), 0o755); err != nil && !u.mockS3 {
		// remote dir only matters for mock local backend
		_ = err
	}
	args := []string{
		"copyto",
		localPath,
		remote,
		"--s3-chunk-size", "16M",
		"--s3-upload-concurrency", "2",
		"--s3-acl", "private",
		"--low-level-retries", "3",
		"--retries", "2",
		"-v",
	}
	if logFile != "" {
		args = append(args, "--log-file", logFile)
	}
	cmd := exec.CommandContext(ctx, "rclone", args...)
	cmd.Env = append(os.Environ(), "RCLONE_CONFIG="+u.configPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("rclone copyto %s: %w: %s", destKey, err, truncate(string(out), 400))
	}
	return nil
}

// UploadJSON writes v as JSON to a small object at destKey (stream index / manifest).
func (u *Uploader) UploadJSON(ctx context.Context, destKey string, v any, logFile string) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal json: %w", err)
	}
	tmp, err := os.CreateTemp("", "seans-idx-*.json")
	if err != nil {
		return fmt.Errorf("temp json: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write json: %w", err)
	}
	tmp.Close()
	return u.UploadFile(ctx, tmp.Name(), destKey, logFile)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}