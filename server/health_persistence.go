package server

import (
	"context"
	"log"
	"sync"
	"time"

	"gondar/db"
)

const maxPendingHealthWrites = 128

type healthWrite struct {
	id       int64
	state    *db.LLMHealth // nil is an ordered manual reset
	sequence uint64
}

// One drain per registry preserves reset/update order. Pending states coalesce
// by profile; the drain exits when idle and every database write has a deadline.
type healthWriter struct {
	mu       sync.Mutex
	pending  map[int64]healthWrite
	running  bool
	sequence uint64
	dropped  uint64
	store    func(context.Context, int64, *db.LLMHealth) error
}

func (w *healthWriter) enqueue(id int64, state *db.LLMHealth) {
	w.mu.Lock()
	if w.pending == nil {
		w.pending = make(map[int64]healthWrite)
	}
	w.sequence++
	w.pending[id] = healthWrite{id: id, state: state, sequence: w.sequence}
	if len(w.pending) > maxPendingHealthWrites {
		oldest := w.pending[id]
		for _, op := range w.pending {
			if op.sequence < oldest.sequence {
				oldest = op
			}
		}
		delete(w.pending, oldest.id)
		w.dropped++
	}
	start := !w.running
	w.running = true
	w.mu.Unlock()
	if start {
		go w.drain()
	}
}

func (w *healthWriter) drain() {
	for {
		w.mu.Lock()
		if len(w.pending) == 0 {
			w.running = false
			dropped := w.dropped
			w.dropped = 0
			w.mu.Unlock()
			if dropped > 0 {
				log.Printf("[llmpool] 健康状态队列超限，合并后丢弃%d份较旧快照，内存状态仍为准", dropped)
			}
			return
		}
		var op healthWrite
		for _, candidate := range w.pending {
			if op.sequence == 0 || candidate.sequence < op.sequence {
				op = candidate
			}
		}
		delete(w.pending, op.id)
		w.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := w.store(ctx, op.id, op.state)
		cancel()
		if err != nil {
			log.Printf("[llmpool] 健康状态落库失败: %v", err)
		}
	}
}
