package app

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	domain "github.com/slidebolt/sb-domain"
	messenger "github.com/slidebolt/sb-messenger-sdk"
	storage "github.com/slidebolt/sb-storage-sdk"
	testkit "github.com/slidebolt/sb-testkit"
)

type harness struct {
	t      *testing.T
	app    *App
	store  storage.Storage
	cmds   *messenger.Commands
	logDir string
}

func start(t *testing.T, listeners string, setup func(h *harness)) *harness {
	t.Helper()
	env := testkit.NewTestEnv(t)
	env.Start("messenger")
	env.Start("storage")
	h := &harness{t: t, store: env.Storage(), cmds: messenger.NewCommands(env.Messenger(), domain.LookupCommand), logDir: t.TempDir()}
	if setup != nil {
		setup(h)
	}
	t.Setenv("INTENT_LISTENERS", listeners)
	t.Setenv("INTENT_LOG_DIR", h.logDir)
	t.Setenv("INTENT_LLM_TIMEOUT_SECONDS", "3")
	h.app = New()
	h.app.pollEvery = 10 * time.Millisecond
	if _, err := h.app.OnStart(map[string]json.RawMessage{"messenger": env.MessengerPayload()}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.app.OnShutdown() })
	return h
}

// button creates an intent button the way a person would: the entity, then its
// phrases written to the user profile.
func (h *harness) button(id, name string, phrases ...string) domain.EntityKey {
	h.t.Helper()
	e := domain.Entity{ID: id, Plugin: PluginID, DeviceID: IntentsDevice, Type: "button", Name: name,
		Commands: []string{"button_press"}, State: domain.Button{}}
	if err := h.store.Save(e); err != nil {
		h.t.Fatal(err)
	}
	key := domain.EntityKey{Plugin: PluginID, DeviceID: IntentsDevice, ID: id}
	profile, _ := json.Marshal(map[string]any{"labels": map[string]any{PhraseLabel: phrases}})
	if err := h.store.SetProfile(key, profile); err != nil {
		h.t.Fatal(err)
	}
	return key
}

func (h *harness) presses(key domain.EntityKey) int {
	h.t.Helper()
	raw, err := h.store.Get(key)
	if err != nil {
		h.t.Fatal(err)
	}
	var e domain.Entity
	json.Unmarshal(raw, &e)
	b, _ := e.State.(domain.Button)
	return b.Presses
}

func listenerKey(name string) domain.EntityKey {
	return domain.EntityKey{Plugin: PluginID, DeviceID: listenerPrefix + name, ID: entityPrompt}
}

func (h *harness) state(name string) ListenerState {
	h.t.Helper()
	raw, err := h.store.Get(listenerKey(name))
	if err != nil {
		h.t.Fatal(err)
	}
	var e struct {
		Type     string        `json:"type"`
		Commands []string      `json:"commands"`
		State    ListenerState `json:"state"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		h.t.Fatal(err)
	}
	return e.State
}

func (h *harness) say(listener, text, session string) {
	h.t.Helper()
	if err := h.cmds.Send(listenerKey(listener), domain.AgentPrompt{Text: text, Session: session}); err != nil {
		h.t.Fatal(err)
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (h *harness) waitIdle(listener string, seq int) ListenerState {
	h.t.Helper()
	eventually(h.t, "listener answer", func() bool {
		s := h.state(listener)
		return s.Seq == seq && s.Status != "thinking"
	})
	return h.state(listener)
}

func (h *harness) waitIntents(n int) {
	h.t.Helper()
	eventually(h.t, "phrase index", func() bool {
		h.app.mu.Lock()
		defer h.app.mu.Unlock()
		return len(h.app.intents) == n
	})
}

func (h *harness) records() []Record {
	h.t.Helper()
	files, _ := filepath.Glob(filepath.Join(h.logDir, "requests-*.jsonl"))
	var out []Record
	for _, name := range files {
		f, err := os.Open(name)
		if err != nil {
			h.t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var r Record
			if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
				h.t.Fatalf("bad record line %q: %v", sc.Text(), err)
			}
			out = append(out, r)
		}
		f.Close()
	}
	return out
}

// fakeAgent stands in for a plugin-opencode agent: it answers each prompt by
// raising Seq and setting the result, and can press a button first, as the LLM does.
type fakeAgent struct {
	mu      sync.Mutex
	prompts []string
	press   *domain.EntityKey
	reply   string
	fail    string
}

func (h *harness) agent(name string) *fakeAgent {
	h.t.Helper()
	f := &fakeAgent{reply: "Done."}
	key := domain.EntityKey{Plugin: "plugin-opencode", DeviceID: "agent_" + name, ID: "prompt"}
	state := domain.Agent{Status: "idle", Seq: 7}
	save := func() {
		if err := h.store.Save(domain.Entity{ID: "prompt", Plugin: "plugin-opencode", DeviceID: "agent_" + name, Type: "agent",
			Name: name, Commands: []string{"agent_prompt", "agent_cancel"}, State: state}); err != nil {
			h.t.Error(err)
		}
	}
	save()
	sub, err := h.cmds.Receive(key.Key()+".>", func(_ messenger.Address, cmd any) {
		p, ok := cmd.(domain.AgentPrompt)
		if !ok {
			return
		}
		f.mu.Lock()
		f.prompts = append(f.prompts, p.Text)
		press, reply, fail := f.press, f.reply, f.fail
		f.mu.Unlock()
		state.Seq++
		state.Status, state.Session, state.Prompt, state.Result, state.Error = "thinking", p.Session, p.Text, "", ""
		save()
		if press != nil {
			h.cmds.Send(*press, domain.ButtonPress{})
			time.Sleep(50 * time.Millisecond)
		}
		state.Status, state.Result = "idle", reply
		if fail != "" {
			state.Status, state.Result, state.Error = "error", "", fail
		}
		save()
	})
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { sub.Unsubscribe() })
	return f
}

func TestListenersComeFromConfig(t *testing.T) {
	h := start(t, "text:agent_text, phone:, BAD NAME:x", nil)
	for _, name := range []string{"text", "phone"} {
		raw, err := h.store.Get(listenerKey(name))
		if err != nil {
			t.Fatalf("listener %s: %v", name, err)
		}
		var e domain.Entity
		json.Unmarshal(raw, &e)
		if e.Type != "agent" || len(e.Commands) != 2 {
			t.Fatalf("listener %s = %+v", name, e)
		}
	}
	if h.app.listeners["text"].fallback != "plugin-opencode.agent_text.prompt" || h.app.listeners["phone"].fallback != "" {
		t.Fatalf("fallbacks = %q, %q", h.app.listeners["text"].fallback, h.app.listeners["phone"].fallback)
	}
	if len(h.app.listeners) != 2 {
		t.Fatalf("a bad name must be ignored: %v", h.app.listeners)
	}
	if _, err := h.store.Get(domain.DeviceKey{Plugin: PluginID, ID: IntentsDevice}); err != nil {
		t.Fatalf("intents device missing: %v", err)
	}
}

func TestPhraseMatchPressesButton(t *testing.T) {
	var off, on domain.EntityKey
	h := start(t, "text:", func(h *harness) {
		off = h.button("porch_off", "Porch off", "turn off the porch", "porch off")
		on = h.button("porch_on", "Porch on", "turn on the porch", "porch on")
	})
	h.waitIntents(2)

	h.say("text", "Please turn the porch off.", "s1")
	s := h.waitIdle("text", 1)
	if s.Tier != TierPhrase || s.Intent != off.Key() || s.Result != "Porch off, done." || s.Status != "idle" {
		t.Fatalf("state = %+v", s)
	}
	eventually(t, "press", func() bool { return h.presses(off) == 1 })

	// The same request again still reaches automations: presses keep counting.
	h.say("text", "porch off", "s1")
	h.waitIdle("text", 2)
	eventually(t, "second press", func() bool { return h.presses(off) == 2 })
	if h.presses(on) != 0 {
		t.Fatal("the opposite intent must not be pressed")
	}
}

func TestNoMatchWithoutFallback(t *testing.T) {
	var off domain.EntityKey
	h := start(t, "text:", func(h *harness) { off = h.button("porch_off", "Porch off", "porch off") })
	h.waitIntents(1)
	h.say("text", "is the porch off", "s1")
	s := h.waitIdle("text", 1)
	if s.Tier != TierNone || s.Result != replyUnknown || s.Status != "idle" {
		t.Fatalf("state = %+v", s)
	}
	if h.presses(off) != 0 {
		t.Fatal("a question must not press anything")
	}
}

func TestMissGoesToAgentAndLinksPressedIntent(t *testing.T) {
	var movie domain.EntityKey
	var ag *fakeAgent
	h := start(t, "text:agent_text", func(h *harness) {
		movie = h.button("movie_time", "Movie time", "it's movie time")
		ag = h.agent("text")
	})
	h.waitIntents(1)
	ag.mu.Lock()
	ag.press, ag.reply = &movie, "I heard 'muddy time'. Starting movie time."
	ag.mu.Unlock()

	h.say("text", "it's muddy time", "text-201")
	s := h.waitIdle("text", 1)
	if s.Tier != TierLLM || s.Result != "I heard 'muddy time'. Starting movie time." || s.Intent != movie.Key() || s.Status != "idle" {
		t.Fatalf("state = %+v", s)
	}
	if h.presses(movie) != 1 {
		t.Fatalf("presses = %d", h.presses(movie))
	}

	recs := h.records()
	if len(recs) != 1 {
		t.Fatalf("records = %+v", recs)
	}
	r := recs[0]
	if r.Tier != TierLLM || r.Text != "it's muddy time" || r.Normalized != "muddy time" || r.Agent != "plugin-opencode.agent_text.prompt" ||
		r.AgentSeq != 8 || len(r.ButtonsPressedDuring) != 1 || r.ButtonsPressedDuring[0] != movie.Key() ||
		len(r.Candidates) != 1 || r.Candidates[0].Key != movie.Key() || r.MinScore != 0.8 || r.Session != "text-201" {
		t.Fatalf("record = %+v", r)
	}
}

func TestAgentErrorIsReported(t *testing.T) {
	var ag *fakeAgent
	h := start(t, "text:agent_text", func(h *harness) { ag = h.agent("text") })
	ag.mu.Lock()
	ag.fail = "timed out"
	ag.mu.Unlock()
	h.say("text", "what's the weather", "s1")
	s := h.waitIdle("text", 1)
	if s.Status != "error" || s.Error != "timed out" || s.Result != replyFailed || s.Tier != TierLLM {
		t.Fatalf("state = %+v", s)
	}
	if recs := h.records(); len(recs) != 1 || recs[0].Error != "timed out" {
		t.Fatalf("records = %+v", recs)
	}
}

func TestPhraseRecordIsWritten(t *testing.T) {
	var off domain.EntityKey
	h := start(t, "phone:", func(h *harness) { off = h.button("porch_off", "Porch off", "porch off") })
	h.waitIntents(1)
	h.say("phone", "porch off", "phone-1")
	h.waitIdle("phone", 1)
	eventually(t, "record", func() bool { return len(h.records()) == 1 })
	r := h.records()[0]
	if r.Tier != TierPhrase || r.Pressed != off.Key() || r.Listener != "phone" || r.Candidates[0].Score != 1 || r.TS.IsZero() {
		t.Fatalf("record = %+v", r)
	}
	if !strings.HasPrefix(filepath.Base(mustGlob(t, h.logDir)), "requests-"+time.Now().UTC().Format("2006-01")) {
		t.Fatal("records go in a monthly file")
	}
}

func mustGlob(t *testing.T, dir string) string {
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("files = %v", files)
	}
	return files[0]
}

func TestIndexFollowsPhraseEdits(t *testing.T) {
	var off domain.EntityKey
	h := start(t, "text:", func(h *harness) { off = h.button("porch_off", "Porch off", "porch off") })
	h.waitIntents(1)
	// New phrase, added later, works without a restart.
	if err := h.store.SetProfile(off, json.RawMessage(`{"labels":{"Phrase":["porch off","kill the porch"]}}`)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "new phrase", func() bool {
		h.app.mu.Lock()
		defer h.app.mu.Unlock()
		return len(h.app.intents[off.Key()].Phrases) == 2
	})
	// Removing every phrase removes the intent.
	if err := h.store.SetProfile(off, json.RawMessage(`{"labels":{}}`)); err != nil {
		t.Fatal(err)
	}
	h.waitIntents(0)
}

func TestNonButtonsAreNotIntents(t *testing.T) {
	if _, ok := toIntent("p.d.light", json.RawMessage(`{"type":"light","commands":["light_turn_on"],"labels":{"Phrase":["lamp on"]}}`)); ok {
		t.Fatal("an entity without button_press must not be an intent")
	}
	it, ok := toIntent("p.d.b", json.RawMessage(`{"type":"button","name":"b","profile":{"name":"Nice name"},"commands":["button_press"],"labels":{"Phrase":["x"]}}`))
	if !ok || it.Name != "Nice name" {
		t.Fatalf("intent = %+v, %v", it, ok)
	}
}
