package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/slidebolt/plugin-intent/internal/match"
	domain "github.com/slidebolt/sb-domain"
)

const defaultSession = "default"

// Replies spoken or texted back when there is no agent to ask.
const (
	replyUnknown = "Sorry, I don't know that one."
	replyFailed  = "Sorry, that didn't work."
)

func parseKey(key string) (domain.EntityKey, error) {
	parts := strings.SplitN(key, ".", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return domain.EntityKey{}, fmt.Errorf("bad entity key %q", key)
	}
	return domain.EntityKey{Plugin: parts[0], DeviceID: parts[1], ID: parts[2]}, nil
}

// handle answers one request: a phrase match presses the button, otherwise the
// fallback agent is asked, otherwise the listener says it doesn't know.
func (a *App) handle(l *listener, p domain.AgentPrompt) {
	started := time.Now()
	session := p.Session
	if session == "" {
		session = defaultSession
	}
	ctx, cancel := context.WithCancel(a.ctx)
	defer cancel()

	l.mu.Lock()
	l.state = ListenerState{Agent: domain.Agent{Status: "thinking", Session: session, Prompt: p.Text, Seq: l.state.Seq + 1}}
	seq := l.state.Seq
	l.cancel = cancel
	l.mu.Unlock()
	a.save(l)

	res := match.Find(p.Text, a.phrases(), a.opt)
	rec := Record{
		Listener: l.name, Session: session, Text: p.Text, Normalized: res.Normalized,
		Candidates: res.Candidates, MinScore: a.opt.MinScore, MinMargin: a.opt.MinMargin,
	}
	var result, intentKey string
	var err error
	switch {
	case res.Matched:
		rec.Tier, intentKey = TierPhrase, res.Key
		rec.Pressed = res.Key
		result, err = a.pressIntent(res.Key)
	case l.fallback != "":
		rec.Tier, rec.Agent = TierLLM, l.fallback
		llmStart := time.Now()
		var agentSeq int
		result, agentSeq, err = a.askAgent(ctx, l.fallback, p.Text, session)
		rec.AgentSeq = agentSeq
		rec.LLMSeconds = time.Since(llmStart).Seconds()
		rec.ButtonsPressedDuring = a.pressesSince(llmStart)
		if len(rec.ButtonsPressedDuring) == 1 {
			intentKey = rec.ButtonsPressedDuring[0]
		}
	default:
		rec.Tier, result = TierNone, replyUnknown
	}
	rec.Seconds = time.Since(started).Seconds()
	rec.Result = result
	if err != nil {
		rec.Error = err.Error()
	}

	l.mu.Lock()
	l.cancel = nil
	if l.state.Seq == seq {
		l.state.Tier, l.state.Intent = rec.Tier, intentKey
		l.state.Result = result
		switch {
		case errors.Is(err, context.Canceled) && a.ctx.Err() == nil:
			l.state.Status, l.state.Error = "idle", "cancelled"
		case err != nil:
			l.state.Status, l.state.Error = "error", err.Error()
		default:
			l.state.Status = "idle"
		}
	}
	l.mu.Unlock()
	if a.ctx.Err() == nil {
		a.save(l)
	}
	a.rec.write(rec)
}

// pressIntent presses a matched intent's button and returns the reply.
func (a *App) pressIntent(key string) (string, error) {
	ek, err := parseKey(key)
	if err != nil {
		return replyFailed, err
	}
	if err := a.cmds.Send(ek, domain.ButtonPress{}); err != nil {
		return replyFailed, fmt.Errorf("press %s: %w", key, err)
	}
	a.mu.Lock()
	name := a.intents[key].Name
	a.mu.Unlock()
	if name == "" {
		name = ek.ID
	}
	return name + ", done.", nil
}

type agentEntity struct {
	State domain.Agent `json:"state"`
}

func (a *App) agentState(key domain.EntityKey) (domain.Agent, error) {
	raw, err := a.store.Get(key)
	if err != nil {
		return domain.Agent{}, err
	}
	var e agentEntity
	if err := json.Unmarshal(raw, &e); err != nil {
		return domain.Agent{}, err
	}
	return e.State, nil
}

// askAgent hands the request to an OpenCode agent and waits for its answer. The
// agent takes prompts in order, so the answer is the first finished state with this
// session and a higher Seq than before the prompt was sent.
func (a *App) askAgent(ctx context.Context, agentKey, text, session string) (string, int, error) {
	ek, err := parseKey(agentKey)
	if err != nil {
		return replyFailed, 0, err
	}
	before, err := a.agentState(ek)
	if err != nil {
		return replyFailed, 0, fmt.Errorf("agent %s: %w", agentKey, err)
	}
	if err := a.cmds.Send(ek, domain.AgentPrompt{Text: text, Session: session}); err != nil {
		return replyFailed, 0, fmt.Errorf("ask %s: %w", agentKey, err)
	}
	ctx, cancel := context.WithTimeout(ctx, a.llmLimit)
	defer cancel()
	tick := time.NewTicker(a.pollEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				return replyFailed, 0, errors.New("timed out waiting for the agent")
			}
			if err := a.cmds.Send(ek, domain.AgentCancel{}); err != nil {
				log.Printf("plugin-intent: cancel %s: %v", agentKey, err)
			}
			return replyFailed, 0, ctx.Err()
		case <-tick.C:
		}
		s, err := a.agentState(ek)
		if err != nil || s.Seq <= before.Seq || s.Session != session || s.Status == "thinking" {
			continue
		}
		if s.Status == "error" || s.Error != "" {
			msg := s.Error
			if msg == "" {
				msg = "agent error"
			}
			return replyFailed, s.Seq, errors.New(msg)
		}
		if s.Result == "" {
			return replyFailed, s.Seq, errors.New("agent gave no answer")
		}
		return s.Result, s.Seq, nil
	}
}
