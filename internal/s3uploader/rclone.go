package s3uploader

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Uploader handles S3 uploads via rclone.
type Uploader struct {
	configPath string
	bucket     string
	logger     *slog.Logger
}

// New creates an Uploader and generates the rclone config file.
func New(stateDir, s3AccessKey, s3SecretKey, s3Endpoint, s3Bucket, s3Region string, logger *slog.Logger) (*Uploader, error) {
	u := &Uploader{
		bucket: s3Bucket,
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
	config := fmt.Sprintf(`[seans]
type = s3
provider = Other
access_key_id = %s
secret_access_key = %s
endpoint = %s
region = %s
force_path_style = true
acl = private
`, s3AccessKey, s3SecretKey, s3Endpoint, s3Region)

	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("mkdir state dir: %w", err)
	}
	return os.WriteFile(u.configPath, []byte(config), 0o600)
}

// remotePath builds the rclone remote path for destPrefix (S3 key prefix).
func (u *Uploader) remotePath(destPrefix string) string {
	// Normalize to forward slashes for object keys.
	destPrefix = strings.ReplaceAll(destPrefix, "\\", "/")
	destPrefix = strings.TrimPrefix(destPrefix, "/")
	return "seans:" + u.bucket + "/" + destPrefix
}

// TransferStats is a parsed rclone --stats snapshot of a running transfer.
type TransferStats struct {
	TransferredBytes int64
	TotalBytes       int64
	Percent          float64
	SpeedBps         int64
	ETASeconds       int64
}

// reStats matches rclone stats lines like:
//
//	Transferred:   1.234 GiB / 2 GiB, 60%, 10.5 MiB/s, ETA 1m20s
var reStats = regexp.MustCompile(`Transferred:\s+([\d.]+)\s*([KMGT]?i?B)\s*/\s*([\d.]+)\s*([KMGT]?i?B),\s*([\d.]+)%,\s*([\d.]+)\s*([KMGT]?i?B)(?:/s|/sec)(?:,\s*ETA\s+([0-9hms]+| -))?`)

// ParseTransferStats parses one rclone log line into TransferStats.
// Second return is false when the line is not a stats line.
func ParseTransferStats(line string) (TransferStats, bool) {
	m := reStats.FindStringSubmatch(line)
	if m == nil {
		return TransferStats{}, false
	}
	st := TransferStats{
		TransferredBytes: parseRcloneBytes(m[1], m[2]),
		TotalBytes:       parseRcloneBytes(m[3], m[4]),
		SpeedBps:         parseRcloneBytes(m[6], m[7]),
	}
	st.Percent, _ = strconv.ParseFloat(m[5], 64)
	if m[8] != "" && strings.TrimSpace(m[8]) != "-" {
		st.ETASeconds = parseRcloneETA(m[8])
	}
	return st, true
}

func parseRcloneBytes(val, unit string) int64 {
	f, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return 0
	}
	switch strings.TrimSpace(unit) {
	case "KiB":
		f *= 1 << 10
	case "MiB":
		f *= 1 << 20
	case "GiB":
		f *= 1 << 30
	case "TiB":
		f *= 1 << 40
	}
	return int64(f)
}

// parseRcloneETA parses durations like "45s", "1m20s", "2h3m", "1h2m3s".
func parseRcloneETA(s string) int64 {
	var total int64
	var num int64
	var hasNum bool
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			num = num*10 + int64(r-'0')
			hasNum = true
		case r == 'h' && hasNum:
			total += num * 3600
			num, hasNum = 0, false
		case r == 'm' && hasNum:
			total += num * 60
			num, hasNum = 0, false
		case r == 's' && hasNum:
			total += num
			num, hasNum = 0, false
		}
	}
	if hasNum {
		total += num
	}
	return total
}

// UploadFile uploads a single local file to destKey (object key / path suffix).
// destKey should use forward slashes, e.g. "users/task/stream/seg_000001".
//
// Segments are uploaded with a single PUT (no multipart): some S3-compatible
// accounts (e.g. buckets.ru) allow PutObject but reject CreateMultipartUpload.
func (u *Uploader) UploadFile(ctx context.Context, localPath, destKey string, logFile string) error {
	return u.UploadFileWithStats(ctx, localPath, destKey, logFile, nil)
}

// UploadFileWithStats uploads a single local file like UploadFile and reports
// rclone transfer stats via cb while the transfer runs (cb may be called many
// times; it must be fast and must not block).
func (u *Uploader) UploadFileWithStats(ctx context.Context, localPath, destKey string, logFile string, cb func(TransferStats)) error {
	remote := u.remotePath(destKey)
	args := []string{
		"copyto",
		localPath,
		remote,
		"--low-level-retries", "3",
		"--retries", "2",
		"--log-level", "INFO",
		"--stats", "5s",
		"--stats-one-line",
		"--stats-log-level", "NOTICE",
	}
	// Force single-part PUT for anything below the cutoff (default stream segment is 8 MiB).
	// Default rclone cutoff is ~5 MiB, so 8 MiB segments would otherwise use multipart and get 403.
	args = append(args,
		"--s3-upload-cutoff", "128M",
		"--s3-no-check-bucket",
	)
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

	scanOut := make(chan string, 64)
	scanErr := make(chan string, 64)
	go scanLines(stdout, scanOut)
	go scanLines(stderr, scanErr)

	done := make(chan struct{})
	var output []string
	appendLine := func(line string) {
		if len(output) < 50 {
			output = append(output, line)
		}
	}

	go func() {
		defer close(done)
		outDone, errDone := false, false
		for !outDone || !errDone {
			select {
			case line, ok := <-scanOut:
				if !ok {
					outDone = true
					continue
				}
				u.logger.Debug("rclone", "out", line)
				appendLine(line)
			case line, ok := <-scanErr:
				if !ok {
					errDone = true
					continue
				}
				if cb != nil {
					if st, ok := ParseTransferStats(line); ok {
						cb(st)
						continue
					}
				}
				if strings.Contains(line, "ERROR") || strings.Contains(line, "error") {
					u.logger.Warn("rclone", "err", line)
				} else {
					u.logger.Debug("rclone", "err", line)
				}
				appendLine(line)
			}
		}
	}()
	<-done

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("rclone copyto -> %s: %w | %s", remote, err, truncate(strings.TrimSpace(strings.Join(output, "\n")), 800))
	}
	// Optional verbose trail for operators.
	if logFile != "" {
		_ = appendFileLine(logFile, "copyto ok: "+localPath+" -> "+remote+"\n")
	}
	return nil
}

// UploadDir copies a local directory tree to the S3 prefix in a single rclone
// run — suitable for many small files (an HLS ladder), unlike per-file copyto.
// The caller keeps and cleans up localDir. Retries up to maxAttempts times.
func (u *Uploader) UploadDir(ctx context.Context, localDir, destPrefix string, maxAttempts int, logFile string) error {
	remote := u.remotePath(destPrefix)
	args := []string{
		"copy",
		localDir,
		remote,
		"--transfers", "4",
		"--checkers", "8",
		"--s3-upload-cutoff", "128M",
		"--s3-no-check-bucket",
		"--fast-list",
		"-v",
		"--stats", "15s",
	}
	if logFile != "" {
		args = append(args, "--log-file", logFile)
	}

	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		cmd := exec.CommandContext(ctx, "rclone", args...)
		cmd.Env = append(os.Environ(), "RCLONE_CONFIG="+u.configPath)
		out, runErr := cmd.CombinedOutput()
		err = runErr
		if runErr == nil {
			u.logger.Info("rclone copy ok", "src", localDir, "dest", remote)
			return nil
		}
		u.logger.Warn("rclone copy attempt failed",
			"attempt", attempt, "max", maxAttempts, "error", runErr, "out", truncate(string(out), 400))
		if attempt < maxAttempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 5 * time.Second):
			}
		}
	}
	return fmt.Errorf("rclone copy %s -> %s failed after %d attempts: %w", localDir, remote, maxAttempts, err)
}

func scanLines(r io.Reader, out chan<- string) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		out <- scanner.Text()
	}
	close(out)
}

func appendFileLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line)
	return err
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
