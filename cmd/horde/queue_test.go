package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/jorge-barreto/horde/internal/store"
)

func TestQueueCmdRegistered(t *testing.T) {
	app := newApp()
	var found bool
	for _, c := range app.Commands {
		if c.Name != "queue" {
			continue
		}
		found = true
		subs := map[string]bool{}
		for _, sc := range c.Commands {
			subs[sc.Name] = true
		}
		for _, want := range []string{"list", "prioritize", "cancel"} {
			if !subs[want] {
				t.Errorf("queue missing subcommand %q", want)
			}
		}
	}
	if !found {
		t.Error("queue command not registered")
	}
}

// seedQueueRun writes a run into the docker-provider SQLite DB used by the
// launch-env harness (~/.horde/horde.db under the temp HOME).
func seedQueueRun(t *testing.T, env launchEnv, run *store.Run) {
	t.Helper()
	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer st.Close()
	if err := st.CreateRun(context.Background(), run); err != nil {
		t.Fatalf("seeding run: %v", err)
	}
}

func getQueueRun(t *testing.T, env launchEnv, id string) *store.Run {
	t.Helper()
	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer st.Close()
	r, err := st.GetRun(context.Background(), id)
	if err != nil {
		t.Fatalf("getting run %s: %v", id, err)
	}
	return r
}

func TestQueuePrioritizeRejectsNonQueued(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()
	now := time.Now()
	seedQueueRun(t, env, &store.Run{
		ID: "running1", Repo: "github.com/test/repo", Ticket: "T-1", Workflow: "w",
		Status: store.StatusRunning, Provider: "docker", LaunchedBy: "me",
		StartedAt: now, TimeoutAt: now.Add(time.Hour),
	})

	app := newApp()
	var buf bytes.Buffer
	setOutputs(app, &buf)
	err := app.Run(ctx, []string{"horde", "--provider", "docker", "queue", "prioritize", "running1", "--priority", "high"})
	if err == nil {
		t.Fatal("expected error reprioritizing a running run, got nil")
	}
}

func TestQueuePrioritizeRequiresPriorityFlag(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()
	now := time.Now()
	seedQueueRun(t, env, &store.Run{
		ID: "q1", Repo: "github.com/test/repo", Ticket: "T-1", Workflow: "w",
		Status: store.StatusQueued, Provider: "docker", LaunchedBy: "me",
		Priority: store.PriorityMed, EnqueuedAt: now, TimeoutAt: now.Add(time.Hour),
	})

	app := newApp()
	var buf bytes.Buffer
	setOutputs(app, &buf)
	err := app.Run(ctx, []string{"horde", "--provider", "docker", "queue", "prioritize", "q1"})
	if err == nil {
		t.Fatal("expected error when --priority omitted, got nil")
	}
}

func TestQueuePrioritizeUpdatesQueuedRun(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()
	now := time.Now()
	seedQueueRun(t, env, &store.Run{
		ID: "q1", Repo: "github.com/test/repo", Ticket: "T-1", Workflow: "w",
		Status: store.StatusQueued, Provider: "docker", LaunchedBy: "me",
		Priority: store.PriorityMed, EnqueuedAt: now, TimeoutAt: now.Add(time.Hour),
	})

	app := newApp()
	var buf bytes.Buffer
	setOutputs(app, &buf)
	if err := app.Run(ctx, []string{"horde", "--provider", "docker", "queue", "prioritize", "q1", "--priority", "highest"}); err != nil {
		t.Fatalf("prioritize: %v", err)
	}
	if got := getQueueRun(t, env, "q1"); got.Priority != store.PriorityHighest {
		t.Errorf("priority = %q, want highest", got.Priority)
	}
}

func TestQueueCancelRejectsNonQueued(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()
	now := time.Now()
	seedQueueRun(t, env, &store.Run{
		ID: "done1", Repo: "github.com/test/repo", Ticket: "T-1", Workflow: "w",
		Status: store.StatusSuccess, Provider: "docker", LaunchedBy: "me",
		StartedAt: now, TimeoutAt: now.Add(time.Hour),
	})

	app := newApp()
	var buf bytes.Buffer
	setOutputs(app, &buf)
	err := app.Run(ctx, []string{"horde", "--provider", "docker", "queue", "cancel", "done1"})
	if err == nil {
		t.Fatal("expected error cancelling a non-queued run, got nil")
	}
}

func TestQueueCancelSetsCancelled(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()
	now := time.Now()
	seedQueueRun(t, env, &store.Run{
		ID: "q1", Repo: "github.com/test/repo", Ticket: "T-1", Workflow: "w",
		Status: store.StatusQueued, Provider: "docker", LaunchedBy: "me",
		Priority: store.PriorityMed, EnqueuedAt: now, TimeoutAt: now.Add(time.Hour),
	})

	app := newApp()
	var buf bytes.Buffer
	setOutputs(app, &buf)
	if err := app.Run(ctx, []string{"horde", "--provider", "docker", "queue", "cancel", "q1"}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got := getQueueRun(t, env, "q1"); got.Status != store.StatusCancelled {
		t.Errorf("status = %q, want cancelled", got.Status)
	}
}

func TestQueueListJSONDrainOrder(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()
	t0 := time.Now().Add(-time.Hour)
	mk := func(id string, p store.Priority, off time.Duration) *store.Run {
		return &store.Run{ID: id, Repo: "github.com/test/repo", Ticket: id, Workflow: "w",
			Status: store.StatusQueued, Provider: "docker", LaunchedBy: "me",
			Priority: p, EnqueuedAt: t0.Add(off), TimeoutAt: t0.Add(time.Hour)}
	}
	seedQueueRun(t, env, mk("low", store.PriorityLow, 0))
	seedQueueRun(t, env, mk("high-new", store.PriorityHigh, 2*time.Minute))
	seedQueueRun(t, env, mk("high-old", store.PriorityHigh, time.Minute))

	app := newApp()
	var buf bytes.Buffer
	setOutputs(app, &buf)
	if err := app.Run(ctx, []string{"horde", "--provider", "docker", "--json", "queue", "list"}); err != nil {
		t.Fatalf("queue list: %v", err)
	}
	var v QueueListV1
	if err := json.Unmarshal(buf.Bytes(), &v); err != nil {
		t.Fatalf("parsing JSON: %v\noutput: %s", err, buf.Bytes())
	}
	want := []string{"high-old", "high-new", "low"}
	if len(v.Queued) != len(want) {
		t.Fatalf("got %d queued, want %d: %+v", len(v.Queued), len(want), v.Queued)
	}
	for i, w := range want {
		if v.Queued[i].RunID != w {
			t.Errorf("drain order[%d] = %q, want %q", i, v.Queued[i].RunID, w)
		}
	}
}
