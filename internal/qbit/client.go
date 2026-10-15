package qbit

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	qbt "github.com/autobrr/go-qbittorrent"
)

type TorrentFile = qbt.TorrentFile
type TorrentInfo = qbt.Torrent

type Client struct {
	qbt *qbt.Client
}

func New(baseURL, apiKey string, logger *slog.Logger) (*Client, error) {
	c := qbt.NewClient(qbt.Config{
		Host:   baseURL,
		APIKey: apiKey,
		Log:    slog.NewLogLogger(logger.Handler(), slog.LevelDebug),
	})
	return &Client{qbt: c}, nil
}

func (c *Client) AddTorrentMagnet(ctx context.Context, magnet, savePath, category string) error {
	_, err := c.qbt.AddTorrentFromUrlCtx(ctx, magnet, map[string]string{
		"savepath": savePath,
		"category": category,
		"paused":   "false",
	})
	return err
}

func (c *Client) AddTorrentFile(ctx context.Context, torrentB64, savePath, category string) error {
	data, err := base64.StdEncoding.DecodeString(torrentB64)
	if err != nil {
		return fmt.Errorf("decode torrent b64: %w", err)
	}
	_, err = c.qbt.AddTorrentFromMemoryCtx(ctx, data, map[string]string{
		"savepath": savePath,
		"category": category,
		"paused":   "false",
	})
	return err
}

func (c *Client) GetTorrents(ctx context.Context, category string) ([]TorrentInfo, error) {
	return c.qbt.GetTorrentsCtx(ctx, qbt.TorrentFilterOptions{
		Category: category,
	})
}

func (c *Client) GetTorrentBySavePath(ctx context.Context, category, savePath string) (*TorrentInfo, error) {
	torrents, err := c.GetTorrents(ctx, category)
	if err != nil {
		return nil, err
	}
	for _, t := range torrents {
		if t.SavePath == savePath {
			return &t, nil
		}
	}
	return nil, nil
}

func (c *Client) GetFiles(ctx context.Context, hash string) ([]TorrentFile, error) {
	files, err := c.qbt.GetFilesInformationCtx(ctx, hash)
	if err != nil {
		return nil, err
	}
	return *files, nil
}

func (c *Client) SetFilePriority(ctx context.Context, hash string, indices []int, priority int) error {
	strs := make([]string, len(indices))
	for i, idx := range indices {
		strs[i] = strconv.Itoa(idx)
	}
	return c.qbt.SetFilePriorityCtx(ctx, hash, strings.Join(strs, "|"), priority)
}

func (c *Client) DeleteTorrent(ctx context.Context, hash string, deleteFiles bool) error {
	return c.qbt.DeleteTorrentsCtx(ctx, []string{hash}, deleteFiles)
}

func (c *Client) PauseTorrents(ctx context.Context, hashes string) error {
	return c.qbt.PauseCtx(ctx, strings.Split(hashes, "|"))
}

// GetPieceStates returns per-piece download states (0=missing, 1=downloading, 2=done).
func (c *Client) GetPieceStates(ctx context.Context, hash string) ([]int, error) {
	states, err := c.qbt.GetTorrentPieceStatesCtx(ctx, hash)
	if err != nil {
		return nil, err
	}
	out := make([]int, len(states))
	for i, s := range states {
		out[i] = int(s)
	}
	return out, nil
}

// GetPieceSize returns torrent piece size in bytes (from torrent properties).
func (c *Client) GetPieceSize(ctx context.Context, hash string) (int64, error) {
	props, err := c.qbt.GetTorrentPropertiesCtx(ctx, hash)
	if err != nil {
		return 0, err
	}
	return int64(props.PieceSize), nil
}

// EnableSequentialDownload turns on sequential piece download if not already on.
// Sequential order is required so contiguous prefix grows and players can stream.
func (c *Client) EnableSequentialDownload(ctx context.Context, hash string) error {
	torrents, err := c.qbt.GetTorrentsCtx(ctx, qbt.TorrentFilterOptions{})
	if err != nil {
		return err
	}
	for _, t := range torrents {
		if t.Hash == hash {
			if t.SequentialDownload {
				return nil
			}
			break
		}
	}
	return c.qbt.ToggleTorrentSequentialDownloadCtx(ctx, []string{hash})
}

// EnableFirstLastPiecePrio prioritizes first/last pieces (container index / duration)
// so media headers become available faster for players.
// Safe to call multiple times: only toggles when currently off.
func (c *Client) EnableFirstLastPiecePrio(ctx context.Context, hash string) error {
	torrents, err := c.qbt.GetTorrentsCtx(ctx, qbt.TorrentFilterOptions{})
	if err != nil {
		return err
	}
	for _, t := range torrents {
		if t.Hash == hash {
			if t.FirstLastPiecePrio {
				return nil
			}
			break
		}
	}
	return c.qbt.ToggleFirstLastPiecePrioCtx(ctx, []string{hash})
}

func TempFile(data []byte, suffix string) (string, error) {
	f, err := os.CreateTemp("", "seans-*"+suffix)
	if err != nil {
		return "", fmt.Errorf("create temp: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("write temp: %w", err)
	}
	f.Close()
	return f.Name(), nil
}

func CategorySavePath(qbitDownloads, taskID string) string {
	return filepath.Join(qbitDownloads, taskID)
}
