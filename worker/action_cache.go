package worker

import (
	"sync"
	"time"

	"github.com/star/mirrorgo/shared"
)

type CachedActionResult struct {
	ActionID   string
	Status     string
	ExitCode   int
	ExitReason string
	StartedAt  time.Time
	FinishedAt time.Time
}

type ActionCache struct {
	mu      sync.Mutex
	items   map[string]*CachedActionResult
	order   []string // oldest first
	maxSize int
}

func NewActionCache(maxSize int) *ActionCache {
	return &ActionCache{
		items:   make(map[string]*CachedActionResult),
		maxSize: maxSize,
	}
}

func (c *ActionCache) Put(result *CachedActionResult) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.items[result.ActionID]; exists {
		// Update existing entry; remove from order so we can re-append.
		for i, id := range c.order {
			if id == result.ActionID {
				c.order = append(c.order[:i], c.order[i+1:]...)
				break
			}
		}
	}

	c.items[result.ActionID] = result
	c.order = append(c.order, result.ActionID)

	// Evict oldest entries if over capacity.
	for len(c.order) > c.maxSize {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.items, oldest)
	}
}

func (c *ActionCache) Get(actionID string) (*CachedActionResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	r, ok := c.items[actionID]
	return r, ok
}

func (c *ActionCache) ToStatusResponse(actionID string) *shared.ActionStatusResponse {
	r, ok := c.Get(actionID)
	if !ok {
		return &shared.ActionStatusResponse{
			Found:    false,
			ActionID: actionID,
		}
	}
	return &shared.ActionStatusResponse{
		Found:      true,
		ActionID:   r.ActionID,
		Status:     r.Status,
		ExitCode:   r.ExitCode,
		ExitReason: r.ExitReason,
		StartedAt:  r.StartedAt,
		FinishedAt: r.FinishedAt,
	}
}
