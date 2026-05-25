package facebook

import (
	"sync"
)

// PageIndex provides a thread-safe lookup from Facebook Page IDs to
// trigger IDs and agent IDs. This enables demultiplexing the single
// shared Facebook webhook endpoint to the correct triggers/agents.
type PageIndex struct {
	mu        sync.RWMutex
	messenger map[string][]string // pageID → triggerIDs (facebook-messenger)
	feed      map[string][]string // pageID → triggerIDs (facebook-feed)
	agents    map[string]string   // pageID → agentID (messenger channel)
}

// NewPageIndex creates an empty page index.
func NewPageIndex() *PageIndex {
	return &PageIndex{
		messenger: make(map[string][]string),
		feed:      make(map[string][]string),
		agents:    make(map[string]string),
	}
}

// AddMessengerTrigger registers a page→trigger mapping for Messenger events.
func (idx *PageIndex) AddMessengerTrigger(pageID, triggerID string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.messenger[pageID] = appendUnique(idx.messenger[pageID], triggerID)
}

// AddFeedTrigger registers a page→trigger mapping for feed events.
func (idx *PageIndex) AddFeedTrigger(pageID, triggerID string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.feed[pageID] = appendUnique(idx.feed[pageID], triggerID)
}

// AddAgent registers a page→agent mapping for Messenger agent channels.
func (idx *PageIndex) AddAgent(pageID, agentID string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.agents[pageID] = agentID
}

// RemoveTrigger removes a trigger ID from all page mappings.
func (idx *PageIndex) RemoveTrigger(triggerID string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	for pageID, ids := range idx.messenger {
		idx.messenger[pageID] = removeFromSlice(ids, triggerID)
		if len(idx.messenger[pageID]) == 0 {
			delete(idx.messenger, pageID)
		}
	}
	for pageID, ids := range idx.feed {
		idx.feed[pageID] = removeFromSlice(ids, triggerID)
		if len(idx.feed[pageID]) == 0 {
			delete(idx.feed, pageID)
		}
	}
}

// RemoveAgent removes an agent from the page index.
func (idx *PageIndex) RemoveAgent(agentID string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for pageID, id := range idx.agents {
		if id == agentID {
			delete(idx.agents, pageID)
		}
	}
}

// LookupMessengerTriggers returns trigger IDs for a page's Messenger events.
func (idx *PageIndex) LookupMessengerTriggers(pageID string) []string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	result := make([]string, len(idx.messenger[pageID]))
	copy(result, idx.messenger[pageID])
	return result
}

// LookupFeedTriggers returns trigger IDs for a page's feed events.
func (idx *PageIndex) LookupFeedTriggers(pageID string) []string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	result := make([]string, len(idx.feed[pageID]))
	copy(result, idx.feed[pageID])
	return result
}

// LookupAgent returns the agent ID for a page's Messenger channel, if any.
func (idx *PageIndex) LookupAgent(pageID string) (string, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	id, ok := idx.agents[pageID]
	return id, ok
}

func appendUnique(slice []string, val string) []string {
	for _, v := range slice {
		if v == val {
			return slice
		}
	}
	return append(slice, val)
}

func removeFromSlice(slice []string, val string) []string {
	result := make([]string, 0, len(slice))
	for _, v := range slice {
		if v != val {
			result = append(result, v)
		}
	}
	return result
}
