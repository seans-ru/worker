package backend

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newTestClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	c := New(srv.URL, "test-secret", testLogger())
	t.Cleanup(srv.Close)
	return c, srv
}

func assertWorkerToken(t *testing.T, r *http.Request, expected string) {
	t.Helper()
	got := r.Header.Get("X-Worker-Token")
	if got != expected {
		t.Errorf("expected X-Worker-Token %q, got %q", expected, got)
	}
}

func TestRegister_HappyPath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/worker/register", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["name"] != "test-host" {
			t.Errorf("expected name test-host, got %v", body["name"])
		}
		if body["reg_secret"] != "test-secret" {
			t.Errorf("expected reg_secret test-secret, got %v", body["reg_secret"])
		}
		respondJSON(w, map[string]any{
			"worker_token": "abc123def456",
		})
	})

	c, _ := newTestClient(t, mux)
	err := c.Register(context.Background(), "test-host")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if !c.IsRegistered() {
		t.Error("expected client to be registered")
	}
}

func TestRegister_BadSecret_401(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/worker/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"detail":"Invalid registration secret"}`))
	})

	c, _ := newTestClient(t, mux)
	err := c.Register(context.Background(), "test-host")
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("expected 401 in error, got: %v", err)
	}
}

func TestClaim_HappyPath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/worker/register", func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, map[string]any{"worker_token": "worker-tok"})
	})
	mux.HandleFunc("POST /api/worker/claim", func(w http.ResponseWriter, r *http.Request) {
		assertWorkerToken(t, r, "worker-tok")
		respondJSON(w, map[string]any{
			"task_id":       "tsk_001",
			"magnet":        "magnet:?xt=urn:btih:abc",
			"media":         map[string]any{"title": "Test Movie"},
			"file_paths":    []string{"movie.mkv"},
			"lease_minutes": 10,
		})
	})

	c, _ := newTestClient(t, mux)
	c.Register(context.Background(), "h")

	task, err := c.Claim(context.Background())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if task == nil {
		t.Fatal("expected task, got nil")
	}
	if task.TaskID != "tsk_001" {
		t.Errorf("expected tsk_001, got %s", task.TaskID)
	}
	if task.Magnet != "magnet:?xt=urn:btih:abc" {
		t.Errorf("expected magnet, got %s", task.Magnet)
	}
	if task.MediaTitle != "Test Movie" {
		t.Errorf("expected Test Movie, got %s", task.MediaTitle)
	}
}

func TestClaim_NoTask(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/worker/register", func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, map[string]any{"worker_token": "worker-tok"})
	})
	mux.HandleFunc("POST /api/worker/claim", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	})

	c, _ := newTestClient(t, mux)
	c.Register(context.Background(), "h")

	task, err := c.Claim(context.Background())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if task != nil {
		t.Errorf("expected nil task, got %+v", task)
	}
}

func TestManifest_HappyPath(t *testing.T) {
	var receivedBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/worker/register", func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, map[string]any{"worker_token": "worker-tok"})
	})
	mux.HandleFunc("POST /api/worker/manifest", func(w http.ResponseWriter, r *http.Request) {
		assertWorkerToken(t, r, "worker-tok")
		data, _ := io.ReadAll(r.Body)
		json.Unmarshal(data, &receivedBody)
		respondJSON(w, map[string]any{
			"task_id": "tsk_001",
			"upload_slots": []map[string]any{
				{"path": "movie.mkv", "s3_key": "users/u1/movie.mkv", "put_url": "https://s3.example.com/presigned"},
			},
		})
	})

	c, _ := newTestClient(t, mux)
	c.Register(context.Background(), "h")

	slots, err := c.Manifest(context.Background(), "tsk_001", []ManifestFile{
		{Path: "movie.mkv", SizeBytes: 1024},
	}, nil)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if receivedBody["task_id"] != "tsk_001" {
		t.Errorf("expected task_id=tsk_001, got %v", receivedBody["task_id"])
	}
	if len(slots) != 1 {
		t.Fatalf("expected 1 slot, got %d", len(slots))
	}
	if slots[0].S3Key != "users/u1/movie.mkv" {
		t.Errorf("expected s3_key users/u1/movie.mkv, got %s", slots[0].S3Key)
	}
}

func TestComplete_HappyPath(t *testing.T) {
	var receivedBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/worker/register", func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, map[string]any{"worker_token": "worker-tok"})
	})
	mux.HandleFunc("POST /api/worker/complete", func(w http.ResponseWriter, r *http.Request) {
		assertWorkerToken(t, r, "worker-tok")
		data, _ := io.ReadAll(r.Body)
		json.Unmarshal(data, &receivedBody)
		respondJSON(w, map[string]any{"ok": true})
	})

	c, _ := newTestClient(t, mux)
	c.Register(context.Background(), "h")

	files := []FileInfo{{Path: "movie.mkv", SizeBytes: 1024}}
	err := c.Complete(context.Background(), "tsk_001", files)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if receivedBody["task_id"] != "tsk_001" {
		t.Errorf("expected task_id=tsk_001, got %v", receivedBody["task_id"])
	}
}

func TestFail_HappyPath(t *testing.T) {
	var receivedBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/worker/register", func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, map[string]any{"worker_token": "worker-tok"})
	})
	mux.HandleFunc("POST /api/worker/fail", func(w http.ResponseWriter, r *http.Request) {
		assertWorkerToken(t, r, "worker-tok")
		data, _ := io.ReadAll(r.Body)
		json.Unmarshal(data, &receivedBody)
		respondJSON(w, map[string]any{"ok": true})
	})

	c, _ := newTestClient(t, mux)
	c.Register(context.Background(), "h")

	err := c.Fail(context.Background(), "tsk_001", "stalled")
	if err != nil {
		t.Fatalf("fail: %v", err)
	}
	if receivedBody["task_id"] != "tsk_001" {
		t.Errorf("expected task_id=tsk_001, got %v", receivedBody["task_id"])
	}
	if receivedBody["error"] != "stalled" {
		t.Errorf("expected error=stalled, got %v", receivedBody["error"])
	}
}

func TestHeartbeat_HappyPath(t *testing.T) {
	var receivedBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/worker/register", func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, map[string]any{"worker_token": "worker-tok"})
	})
	mux.HandleFunc("POST /api/worker/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		assertWorkerToken(t, r, "worker-tok")
		data, _ := io.ReadAll(r.Body)
		json.Unmarshal(data, &receivedBody)
		respondJSON(w, map[string]any{"ok": true})
	})

	c, _ := newTestClient(t, mux)
	c.Register(context.Background(), "h")

	hb := HeartbeatTask{TaskID: "tsk_001", Stage: "downloading", ProgressPct: 50.0, SpeedBps: 1572864}
	err := c.Heartbeat(context.Background(), hb)
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if receivedBody["task_id"] != "tsk_001" {
		t.Errorf("expected task_id=tsk_001, got %v", receivedBody["task_id"])
	}
	if receivedBody["stage"] != "downloading" {
		t.Errorf("expected stage=downloading, got %v", receivedBody["stage"])
	}
}

func Test401_ClearsToken(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/worker/register", func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, map[string]any{"worker_token": "worker-tok"})
	})
	mux.HandleFunc("POST /api/worker/claim", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"detail":"unauthorized"}`))
	})

	c, _ := newTestClient(t, mux)
	c.Register(context.Background(), "h")

	_, err := c.Claim(context.Background())
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("expected 401 in error, got: %v", err)
	}
}

func respondJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}
