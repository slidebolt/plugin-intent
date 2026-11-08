// Package app turns spoken or typed requests into button presses. Each intent is a
// button carrying its phrases in the Phrase label; whatever automation watches the
// button does the work. A listener device per channel takes the request: a phrase
// match presses the button straight away, anything else goes to an OpenCode agent
// when one is configured.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/slidebolt/plugin-intent/internal/match"
	contract "github.com/slidebolt/sb-contract"
	domain "github.com/slidebolt/sb-domain"
	logging "github.com/slidebolt/sb-logging-sdk"
	messenger "github.com/slidebolt/sb-messenger-sdk"
	storage "github.com/slidebolt/sb-storage-sdk"
)

const (
	PluginID = "plugin-intent"

	// IntentsDevice holds the intent buttons people create.
	IntentsDevice = "intents"

	listenerPrefix = "listener_"
	entityPrompt   = "prompt"
	queueSize      = 8

	PhraseLabel = "Phrase"
)

var (
	listenerCommands = []string{"agent_prompt", "agent_cancel"}
	listenerName     = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
)

// Tiers say how a request was answered.
const (
	TierPhrase = "phrase" // a phrase matched and the button was pressed
	TierLLM    = "llm"    // handed to the fallback agent
	TierNone   = "none"   // no match and no fallback agent
)

// ListenerState is domain.Agent plus how the last request was answered, so the
// reply scripts written for OpenCode agents work unchanged.
type ListenerState struct {
	domain.Agent
	Tier   string `json:"tier,omitempty"`
	Intent string `json:"intent,omitempty"`
}

type listener struct {
	name     string
	fallback string // entity key of the OpenCode agent, or ""
	queue    chan domain.AgentPrompt

	mu     sync.Mutex
	state  ListenerState
	cancel context.CancelFunc
}

type App struct {
	msg    messenger.Messenger
	store  storage.Storage
	cmds   *messenger.Commands
	logger logging.Store
	subs   []messenger.Subscription
	watch  *storage.Watcher
	rec    *recorder

	opt       match.Options
	llmLimit  time.Duration
	pollEvery time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	listeners map[string]*listener

	mu      sync.Mutex
	intents map[string]intent // entity key -> intent
	presses []press           // recent presses, for linking an LLM answer to an intent
}

type intent struct {
	Key     string
	Name    string
	Phrases []string
}

type press struct {
	key string
	at  time.Time
}

func New() *App {
	return &App{listeners: map[string]*listener{}, intents: map[string]intent{}}
}

func (a *App) Hello() contract.HelloResponse {
	return contract.HelloResponse{
		ID:              PluginID,
		Kind:            contract.KindPlugin,
		ContractVersion: contract.ContractVersion,
		DependsOn:       []string{"messenger", "storage"},
	}
}

func envFloat(name string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(name), 64); err == nil && v >= 0 {
		return v
	}
	return def
}

// parseListeners reads "text:agent_text,phone:agent_phone,kiosk:". The value names a
// plugin-opencode agent ("agent_text"), a full entity key, or nothing for no fallback.
func parseListeners(spec string) map[string]string {
	out := map[string]string{}
	for _, item := range strings.Split(spec, ",") {
		name, agent, _ := strings.Cut(strings.TrimSpace(item), ":")
		name, agent = strings.TrimSpace(name), strings.TrimSpace(agent)
		if name == "" {
			continue
		}
		if !listenerName.MatchString(name) {
			log.Printf("plugin-intent: ignoring listener with a bad name: %q", name)
			continue
		}
		if agent != "" && !strings.Contains(agent, ".") {
			agent = "plugin-opencode." + agent + "." + entityPrompt
		}
		out[name] = agent
	}
	return out
}

// OnStart stays up when parts are missing; a failed start would stop every plugin.
func (a *App) OnStart(deps map[string]json.RawMessage) (json.RawMessage, error) {
	msg, err := messenger.Connect(deps)
	if err != nil {
		return nil, fmt.Errorf("connect messenger: %w", err)
	}
	a.msg = msg
	store, err := storage.Connect(deps)
	if err != nil {
		return nil, fmt.Errorf("connect storage: %w", err)
	}
	a.store = store
	if logger, err := logging.Connect(deps); err == nil {
		a.logger = logger
	} else {
		log.Printf("plugin-intent: logging not available: %v", err)
	}
	a.cmds = messenger.NewCommands(msg, domain.LookupCommand)
	a.ctx, a.cancel = context.WithCancel(context.Background())

	a.opt = match.Options{MinScore: envFloat("INTENT_MIN_SCORE", 0.8), MinMargin: envFloat("INTENT_MIN_MARGIN", 0.15)}
	a.llmLimit = time.Duration(envFloat("INTENT_LLM_TIMEOUT_SECONDS", 300) * float64(time.Second))
	if a.pollEvery == 0 {
		a.pollEvery = 200 * time.Millisecond
	}
	dir := os.Getenv("INTENT_LOG_DIR")
	if dir == "" {
		dir = ".data/intent"
	}
	a.rec = &recorder{dir: dir, logger: a.logger}

	if err := a.ensureIntentsDevice(); err != nil {
		log.Printf("plugin-intent: %v", err)
	}
	if err := a.watchIntents(); err != nil {
		log.Printf("plugin-intent: phrase index not available: %v", err)
	}

	sub, err := a.cmds.Receive(PluginID+".>", a.handleCommand)
	if err != nil {
		return nil, fmt.Errorf("subscribe commands: %w", err)
	}
	a.subs = append(a.subs, sub)

	for name, fallback := range parseListeners(os.Getenv("INTENT_LISTENERS")) {
		l := &listener{name: name, fallback: fallback, queue: make(chan domain.AgentPrompt, queueSize),
			state: ListenerState{Agent: domain.Agent{Status: "idle"}}}
		a.listeners[name] = l
		a.save(l)
		a.wg.Add(1)
		go a.worker(l)
	}
	log.Printf("plugin-intent: started with %d listeners and %d intents (min score %.2f, min margin %.2f)",
		len(a.listeners), len(a.intents), a.opt.MinScore, a.opt.MinMargin)
	return nil, nil
}

func (a *App) OnShutdown() error {
	if a.cancel != nil {
		a.cancel()
	}
	for _, sub := range a.subs {
		sub.Unsubscribe()
	}
	if a.watch != nil {
		a.watch.Stop()
	}
	a.wg.Wait()
	if a.store != nil {
		a.store.Close()
	}
	if a.msg != nil {
		a.msg.Close()
	}
	return nil
}

func (a *App) ensureIntentsDevice() error {
	key := domain.DeviceKey{Plugin: PluginID, ID: IntentsDevice}
	if _, err := a.store.Get(key); err == nil {
		return nil
	}
	if err := a.store.Save(domain.Device{ID: IntentsDevice, Plugin: PluginID, Name: "Intents"}); err != nil {
		return fmt.Errorf("save intents device: %w", err)
	}
	return nil
}

func (a *App) save(l *listener) {
	l.mu.Lock()
	state := l.state
	l.mu.Unlock()
	d := listenerPrefix + l.name
	prompt := domain.Entity{
		ID: entityPrompt, Plugin: PluginID, DeviceID: d, Type: "agent", Name: "Listener " + l.name,
		Commands: listenerCommands, State: state,
	}
	device := domain.Device{ID: d, Plugin: PluginID, Name: "Listener " + l.name, Entities: []domain.Entity{prompt}}
	for _, v := range []storage.Keyed{device, prompt} {
		if err := a.store.Save(v); err != nil {
			log.Printf("plugin-intent: save %s: %v", v.Key(), err)
		}
	}
}

func (a *App) handleCommand(addr messenger.Address, cmd any) {
	if name, ok := strings.CutPrefix(addr.DeviceID, listenerPrefix); ok && addr.EntityID == entityPrompt {
		l := a.listeners[name]
		if l == nil {
			log.Printf("plugin-intent: unknown listener %q", name)
			return
		}
		switch c := cmd.(type) {
		case domain.AgentPrompt:
			a.enqueue(l, c)
		case domain.AgentCancel:
			l.mu.Lock()
			cancel := l.cancel
			l.mu.Unlock()
			if cancel != nil {
				cancel()
			}
		}
		return
	}
	if _, ok := cmd.(domain.ButtonPress); ok {
		if err := a.pressButton(addr); err != nil {
			log.Printf("plugin-intent: press %s: %v", addr.Key(), err)
		}
	}
}

// pressButton counts a press on one of this plugin's buttons, so automations that
// watch it fire even when the same intent is asked for twice in a row.
func (a *App) pressButton(addr messenger.Address) error {
	raw, err := a.store.Get(domain.EntityKey{Plugin: addr.Plugin, DeviceID: addr.DeviceID, ID: addr.EntityID})
	if err != nil {
		return fmt.Errorf("load: %w", err)
	}
	var e domain.Entity
	if err := json.Unmarshal(raw, &e); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if e.Type != "button" {
		return fmt.Errorf("type %q is not a button", e.Type)
	}
	state, _ := e.State.(domain.Button)
	state.Presses++
	e.State = state
	a.mu.Lock()
	a.presses = append(a.presses, press{key: addr.Key(), at: time.Now()})
	if len(a.presses) > 100 {
		a.presses = a.presses[len(a.presses)-100:]
	}
	a.mu.Unlock()
	return a.store.Save(e)
}

// pressesSince lists the buttons of this plugin pressed after t, oldest first.
func (a *App) pressesSince(t time.Time) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, p := range a.presses {
		if !p.at.Before(t) {
			out = append(out, p.key)
		}
	}
	return out
}

func (a *App) enqueue(l *listener, p domain.AgentPrompt) {
	if strings.TrimSpace(p.Text) == "" {
		return
	}
	select {
	case l.queue <- p:
	default:
		l.mu.Lock()
		l.state.Error = "too many requests waiting"
		l.mu.Unlock()
		a.save(l)
	}
}

func (a *App) worker(l *listener) {
	defer a.wg.Done()
	for {
		select {
		case <-a.ctx.Done():
			return
		case p := <-l.queue:
			a.handle(l, p)
		}
	}
}
