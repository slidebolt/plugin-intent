package app

import (
	"encoding/json"
	"fmt"
	"log"
	"slices"

	"github.com/slidebolt/plugin-intent/internal/match"
	domain "github.com/slidebolt/sb-domain"
	storage "github.com/slidebolt/sb-storage-sdk"
)

// phraseQuery finds every entity with phrases, whichever plugin owns it.
var phraseQuery = storage.Query{Where: []storage.Filter{{Field: "labels." + PhraseLabel, Op: storage.Exists}}}

// toIntent reads an entity's phrases. Only entities that can be pressed count.
func toIntent(key string, data json.RawMessage) (intent, bool) {
	var e domain.Entity
	if err := json.Unmarshal(data, &e); err != nil {
		return intent{}, false
	}
	phrases := e.Labels[PhraseLabel]
	if len(phrases) == 0 || !slices.Contains(e.Commands, "button_press") {
		return intent{}, false
	}
	name := e.Name
	if e.Profile != nil && e.Profile.Name != "" {
		name = e.Profile.Name
	}
	return intent{Key: key, Name: name, Phrases: phrases}, true
}

func (a *App) upsertIntent(key string, data json.RawMessage) {
	it, ok := toIntent(key, data)
	a.mu.Lock()
	defer a.mu.Unlock()
	if ok {
		a.intents[key] = it
	} else {
		delete(a.intents, key)
	}
}

func (a *App) removeIntent(key string, _ json.RawMessage) {
	a.mu.Lock()
	delete(a.intents, key)
	a.mu.Unlock()
}

// watchIntents keeps the phrase index current, the same way plugin-alexa follows
// entities labelled PluginAlexa.
func (a *App) watchIntents() error {
	w, err := storage.Watch(a.msg, phraseQuery, storage.WatchHandlers{
		OnAdd:    a.upsertIntent,
		OnUpdate: a.upsertIntent,
		OnRemove: a.removeIntent,
	})
	if err != nil {
		return fmt.Errorf("watch phrases: %w", err)
	}
	a.watch = w
	entries, err := a.store.Query(phraseQuery)
	if err != nil {
		return fmt.Errorf("query phrases: %w", err)
	}
	for _, e := range entries {
		w.Populate(e.Key, e.Data)
		a.upsertIntent(e.Key, e.Data)
	}
	log.Printf("plugin-intent: indexed %d intents", len(a.intents))
	return nil
}

func (a *App) phrases() []match.Phrase {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []match.Phrase
	for key, it := range a.intents {
		for _, p := range it.Phrases {
			out = append(out, match.Phrase{Key: key, Text: p})
		}
	}
	return out
}
