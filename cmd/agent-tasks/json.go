package main

import (
	"time"

	"github.com/danmestas/agent-infra/coord"
)

type coordTaskJSON struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Files     []string  `json:"files,omitempty"`
	ClaimedBy string    `json:"claimed_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type chatThreadJSON struct {
	ThreadShort  string    `json:"thread_short"`
	LastActivity time.Time `json:"last_activity"`
	MessageCount int       `json:"message_count"`
	LastBody     string    `json:"last_body"`
}

type presenceJSON struct {
	AgentID   string    `json:"agent_id"`
	Project   string    `json:"project"`
	StartedAt time.Time `json:"started_at"`
	LastSeen  time.Time `json:"last_seen"`
}

type primeResultJSON struct {
	OpenTasks    []coordTaskJSON  `json:"open_tasks"`
	ReadyTasks   []coordTaskJSON  `json:"ready_tasks"`
	ClaimedTasks []coordTaskJSON  `json:"claimed_tasks"`
	Threads      []chatThreadJSON `json:"threads"`
	Peers        []presenceJSON   `json:"peers"`
}

func coordTaskToJSON(t coord.Task) coordTaskJSON {
	return coordTaskJSON{
		ID:        string(t.ID()),
		Title:     t.Title(),
		Files:     t.Files(),
		ClaimedBy: t.ClaimedBy(),
		CreatedAt: t.CreatedAt(),
		UpdatedAt: t.UpdatedAt(),
	}
}

func coordTasksToJSON(ts []coord.Task) []coordTaskJSON {
	out := make([]coordTaskJSON, 0, len(ts))
	for _, t := range ts {
		out = append(out, coordTaskToJSON(t))
	}
	return out
}

func primeToJSON(r coord.PrimeResult) primeResultJSON {
	out := primeResultJSON{
		OpenTasks:    coordTasksToJSON(r.OpenTasks),
		ReadyTasks:   coordTasksToJSON(r.ReadyTasks),
		ClaimedTasks: coordTasksToJSON(r.ClaimedTasks),
		Threads:      make([]chatThreadJSON, 0, len(r.Threads)),
		Peers:        make([]presenceJSON, 0, len(r.Peers)),
	}
	for _, t := range r.Threads {
		out.Threads = append(out.Threads, chatThreadJSON{
			ThreadShort:  t.ThreadShort(),
			LastActivity: t.LastActivity(),
			MessageCount: t.MessageCount(),
			LastBody:     t.LastBody(),
		})
	}
	for _, p := range r.Peers {
		out.Peers = append(out.Peers, presenceJSON{
			AgentID:   p.AgentID(),
			Project:   p.Project(),
			StartedAt: p.StartedAt(),
			LastSeen:  p.LastSeen(),
		})
	}
	return out
}
