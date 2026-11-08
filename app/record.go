package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/slidebolt/plugin-intent/internal/match"
	logging "github.com/slidebolt/sb-logging-sdk"
)

// Record is one request and how it was answered: one line in the tuning log.
type Record struct {
	TS                   time.Time         `json:"ts"`
	Listener             string            `json:"listener"`
	Session              string            `json:"session"`
	Text                 string            `json:"text"`
	Normalized           string            `json:"normalized"`
	Candidates           []match.Candidate `json:"candidates"`
	MinScore             float64           `json:"min_score"`
	MinMargin            float64           `json:"min_margin"`
	Tier                 string            `json:"tier"`
	Pressed              string            `json:"pressed,omitempty"`
	Agent                string            `json:"agent,omitempty"`
	AgentSeq             int               `json:"agent_seq,omitempty"`
	LLMSeconds           float64           `json:"llm_seconds,omitempty"`
	ButtonsPressedDuring []string          `json:"buttons_pressed_during,omitempty"`
	Result               string            `json:"result"`
	Error                string            `json:"error,omitempty"`
	Seconds              float64           `json:"seconds"`
}

// recorder appends records to a monthly JSONL file kept with the data, so it is
// backed up and never pruned, and sends each one to sb-logging for a live view.
type recorder struct {
	dir    string
	logger logging.Store
	mu     sync.Mutex
}

var logSeq uint64

func (r *recorder) write(rec Record) {
	if rec.TS.IsZero() {
		rec.TS = time.Now().UTC()
	}
	line, err := json.Marshal(rec)
	if err != nil {
		log.Printf("plugin-intent: encode record: %v", err)
		return
	}
	if err := r.append(rec.TS, line); err != nil {
		log.Printf("plugin-intent: write record: %v", err)
	}
	if r.logger == nil {
		return
	}
	var data map[string]any
	json.Unmarshal(line, &data)
	event := logging.Event{
		ID:      fmt.Sprintf("%s-%d-%d", PluginID, time.Now().UnixNano(), atomic.AddUint64(&logSeq, 1)),
		TS:      rec.TS,
		Source:  PluginID,
		Kind:    "intent.resolved",
		Level:   "info",
		Message: fmt.Sprintf("%s: %q -> %s %s", rec.Listener, rec.Text, rec.Tier, rec.Pressed),
		Plugin:  PluginID,
		Device:  listenerPrefix + rec.Listener,
		Data:    data,
	}
	if rec.Error != "" {
		event.Level = "warn"
	}
	if err := r.logger.Append(context.Background(), event); err != nil {
		log.Printf("plugin-intent: log record: %v", err)
	}
}

func (r *recorder) append(ts time.Time, line []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return err
	}
	name := filepath.Join(r.dir, "requests-"+ts.UTC().Format("2006-01")+".jsonl")
	f, err := os.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}
