package engine

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Geardung/seans-worker/internal/backend"
	"github.com/Geardung/seans-worker/internal/config"
	"github.com/Geardung/seans-worker/internal/qbit"
	"github.com/Geardung/seans-worker/internal/s3uploader"
)

// Engine is the main worker loop.
type Engine struct {
	cfg      *config.Config
	bc       *backend.Client
	qc       *qbit.Client
	uploader *s3uploader.Uploader
	logger   *slog.Logger

	mu          sync.Mutex
	activeTasks map[string]*TaskState
	claimCancel context.CancelFunc
}

func New(cfg *config.Config, bc *backend.Client, qc *qbit.Client, uploader *s3uploader.Uploader, logger *slog.Logger) *Engine {
	return &Engine{
		cfg:         cfg,
		bc:          bc,
		qc:          qc,
		uploader:    uploader,
		logger:      logger,
		activeTasks: make(map[string]*TaskState),
	}
}

// Run starts the worker loop. Blocks until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	heartbeatCtx, heartbeatCancel := context.WithCancel(ctx)
	defer heartbeatCancel()
	go e.heartbeatLoop(heartbeatCtx)

	claimCtx, claimCancel := context.WithCancel(ctx)
	e.mu.Lock()
	e.claimCancel = claimCancel
	e.mu.Unlock()

	e.logger.Info("claim loop started", "interval", e.cfg.ClaimInterval)
	ticker := time.NewTicker(e.cfg.ClaimInterval)
	defer ticker.Stop()

	for {
		select {
		case <-claimCtx.Done():
			e.logger.Info("claim loop stopped")
			return
		case <-ticker.C:
			e.tryClaim(claimCtx)
		}
	}
}

// Shutdown stops the engine gracefully.
func (e *Engine) Shutdown(ctx context.Context) {
	e.logger.Info("shutdown initiated", "drain", e.cfg.DrainOnShutdown)

	e.mu.Lock()
	if e.claimCancel != nil {
		e.claimCancel()
	}
	e.mu.Unlock()

	if e.cfg.DrainOnShutdown {
		e.logger.Info("draining active tasks", "timeout", e.cfg.DrainTimeout)
		drainCtx, drainCancel := context.WithTimeout(ctx, e.cfg.DrainTimeout)
		defer drainCancel()
		e.drain(drainCtx)
	} else {
		e.failAllActive(ctx, "worker shutting down")
	}

	e.sendHeartbeats(ctx)
	e.logger.Info("shutdown complete")
}

func (e *Engine) tryClaim(ctx context.Context) {
	e.mu.Lock()
	active := len(e.activeTasks)
	e.mu.Unlock()

	if active >= e.cfg.MaxConcurrentTasks {
		return
	}

	task, err := e.bc.Claim(ctx)
	if err != nil {
		e.logger.Warn("claim failed", "error", err)
		return
	}
	if task == nil {
		e.logger.Debug("no tasks available")
		return
	}

	e.logger.Info("claimed task", "task_id", task.TaskID, "title", task.MediaTitle)

	state := &TaskState{
		TaskID:          task.TaskID,
		Stage:           StagePreparing,
		StartedAt:       time.Now(),
		StreamFileIndex: -1,
	}

	e.mu.Lock()
	e.activeTasks[task.TaskID] = state
	e.mu.Unlock()

	go func() {
		runner := newTaskRunner(e.cfg, e.bc, e.qc, e.uploader, task, state, e.logger)
		runner.run(ctx)

		e.mu.Lock()
		delete(e.activeTasks, task.TaskID)
		e.mu.Unlock()
	}()
}

func (e *Engine) heartbeatLoop(ctx context.Context) {
	interval := e.cfg.HeartbeatInterval
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	consecutiveErrors := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := e.sendHeartbeats(ctx); err != nil {
				consecutiveErrors++
				if consecutiveErrors >= 3 {
					e.logger.Error("heartbeat failed 3 consecutive times", "error", err)
				} else {
					e.logger.Warn("heartbeat failed", "error", err, "consecutive", consecutiveErrors)
				}
			} else {
				consecutiveErrors = 0
			}
		}
	}
}

func (e *Engine) sendHeartbeats(ctx context.Context) error {
	e.mu.Lock()
	snapshot := make(map[string]*TaskState, len(e.activeTasks))
	for k, v := range e.activeTasks {
		snapshot[k] = v
	}
	e.mu.Unlock()

	if len(snapshot) == 0 {
		return nil
	}

	for _, s := range snapshot {
		hb := backend.HeartbeatTask{
			TaskID:      s.TaskID,
			Stage:       string(s.Stage),
			ProgressPct: s.ProgressPct,
			SpeedBps:    int64(s.SpeedMbps * 1024 * 1024),
			Stream:      s.Stream,
		}
		if err := e.bc.Heartbeat(ctx, hb); err != nil {
			e.logger.Warn("heartbeat failed for task", "task_id", s.TaskID, "error", err)
		}
	}

	e.logger.Debug("heartbeats sent", "tasks", len(snapshot))
	return nil
}

func (e *Engine) drain(ctx context.Context) {
	for {
		e.mu.Lock()
		active := len(e.activeTasks)
		e.mu.Unlock()

		if active == 0 {
			return
		}

		select {
		case <-ctx.Done():
			e.logger.Warn("drain timeout reached, failing remaining tasks")
			e.failAllActive(ctx, "drain timeout")
			return
		case <-time.After(1 * time.Second):
		}
	}
}

func (e *Engine) failAllActive(ctx context.Context, reason string) {
	e.mu.Lock()
	ids := make([]string, 0, len(e.activeTasks))
	for id := range e.activeTasks {
		ids = append(ids, id)
	}
	e.mu.Unlock()

	for _, id := range ids {
		if err := e.bc.Fail(ctx, id, reason); err != nil {
			e.logger.Error("failed to report failure during shutdown", "task_id", id, "error", err)
		}
	}

	hashes := ""
	e.mu.Lock()
	for _, s := range e.activeTasks {
		if s.TorrentHash != "" {
			if hashes != "" {
				hashes += "|"
			}
			hashes += s.TorrentHash
		}
	}
	e.mu.Unlock()

	if hashes != "" {
		if err := e.qc.PauseTorrents(ctx, hashes); err != nil {
			e.logger.Warn("failed to pause torrents during shutdown", "error", err)
		}
	}
}

func (e *Engine) ActiveTasks() []TaskState {
	e.mu.Lock()
	defer e.mu.Unlock()
	states := make([]TaskState, 0, len(e.activeTasks))
	for _, s := range e.activeTasks {
		states = append(states, *s)
	}
	return states
}
