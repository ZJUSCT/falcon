package master

import "sync"

type Queue struct {
	items []string
	idx   int

	mu             sync.Mutex
	notEmpty       *sync.Cond
	paused         bool
	MaxConcurrency int
}

func NewQueue() *Queue {
	q := &Queue{
		MaxConcurrency: 1, // 默认最大并发数为1
	}
	q.notEmpty = sync.NewCond(&q.mu)
	return q
}

// Enqueue adds an item to the end of the queue
func (q *Queue) Enqueue(id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append(q.items, id)
	q.notEmpty.Signal()
}

// Dequeue removes and returns the item from the front of the queue
// If the queue is empty, returns empty string and false
func (q *Queue) Dequeue() (string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.paused {
		return "", false
	}
	if q.idx >= len(q.items) {
		return "", false
	}
	val := q.items[q.idx]
	q.idx++
	// Compact slice periodically to avoid unbounded growth
	if q.idx > 64 && q.idx*2 >= len(q.items) {
		q.items = append([]string(nil), q.items[q.idx:]...)
		q.idx = 0
	}
	return val, true
}

// Remove removes the first occurrence of id from the queue if present
func (q *Queue) Remove(id string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := q.idx; i < len(q.items); i++ {
		if q.items[i] == id {
			copy(q.items[i:], q.items[i+1:])
			q.items = q.items[:len(q.items)-1]
			return true
		}
	}
	return false
}

func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items) - q.idx
}

// Snapshot returns a copy of the queue from head to tail for read-only use
func (q *Queue) Snapshot() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.idx >= len(q.items) {
		return nil
	}
	out := make([]string, len(q.items)-q.idx)
	copy(out, q.items[q.idx:])
	return out
}

// ReplaceAll replaces the contents of the queue with the provided items
// in order, resetting the head index.
func (q *Queue) ReplaceAll(items []string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append([]string(nil), items...)
	q.idx = 0
}

func (q *Queue) SetPaused(p bool) {
	q.mu.Lock()
	q.paused = p
	q.mu.Unlock()
}

func (q *Queue) IsPaused() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.paused
}

func (q *Queue) SetMaxConcurrency(max int) {
	q.mu.Lock()
	q.MaxConcurrency = max
	q.mu.Unlock()
}

func (q *Queue) GetMaxConcurrency() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.MaxConcurrency
}

// MoveToHead moves the job to the front (next to be dequeued)
func (q *Queue) MoveToHead(id string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	pos := -1
	for i := q.idx; i < len(q.items); i++ {
		if q.items[i] == id {
			pos = i
			break
		}
	}
	if pos == -1 {
		return false
	}
	val := q.items[pos]
	copy(q.items[pos:], q.items[pos+1:])
	q.items = q.items[:len(q.items)-1]
	q.items = append(q.items[:q.idx], append([]string{val}, q.items[q.idx:]...)...)
	return true
}

// MoveToTail moves the job to the end of the queue
func (q *Queue) MoveToTail(id string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	pos := -1
	for i := q.idx; i < len(q.items); i++ {
		if q.items[i] == id {
			pos = i
			break
		}
	}
	if pos == -1 {
		return false
	}
	val := q.items[pos]
	copy(q.items[pos:], q.items[pos+1:])
	q.items = q.items[:len(q.items)-1]
	q.items = append(q.items, val)
	return true
}

// MoveBefore moves target before ref in the pending window
func (q *Queue) MoveBefore(targetID, refID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	pending := append([]string(nil), q.items[q.idx:]...)
	tPos, rPos := -1, -1
	for i, id := range pending {
		if id == targetID {
			tPos = i
		}
		if id == refID {
			rPos = i
		}
	}
	if tPos == -1 || rPos == -1 || tPos == rPos {
		return false
	}
	// remove target
	val := pending[tPos]
	copy(pending[tPos:], pending[tPos+1:])
	pending = pending[:len(pending)-1]
	// find new ref index (if target was before ref, ref shifts -1)
	if tPos < rPos {
		rPos--
	}
	// insert before ref
	pending = append(pending[:rPos], append([]string{val}, pending[rPos:]...)...)
	q.items = append(q.items[:q.idx], pending...)
	return true
}

// MoveAfter moves target after ref in the pending window
func (q *Queue) MoveAfter(targetID, refID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	pending := append([]string(nil), q.items[q.idx:]...)
	tPos, rPos := -1, -1
	for i, id := range pending {
		if id == targetID {
			tPos = i
		}
		if id == refID {
			rPos = i
		}
	}
	if tPos == -1 || rPos == -1 || tPos == rPos {
		return false
	}
	val := pending[tPos]
	copy(pending[tPos:], pending[tPos+1:])
	pending = pending[:len(pending)-1]
	// if target was before ref, ref shifts -1
	if tPos < rPos {
		rPos--
	}
	insertPos := rPos + 1
	if insertPos > len(pending) {
		insertPos = len(pending)
	}
	pending = append(pending[:insertPos], append([]string{val}, pending[insertPos:]...)...)
	q.items = append(q.items[:q.idx], pending...)
	return true
}
