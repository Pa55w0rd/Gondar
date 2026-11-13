package server

import (
	"sync"
	"sync/atomic"
)

// File readers join the deletion barrier without becoming agent executions.
// Goal/config edits and ordinary execution drain must not wait on a download.
func (e *Engine) beginTaskFileRead(taskID string) (func(), bool) {
	e.deleteMu.RLock()
	defer e.deleteMu.RUnlock()
	if e.IsDeleting(taskID) {
		return nil, false
	}
	value, _ := e.taskReads.LoadOrStore(taskID, new(int64))
	counter := value.(*int64)
	atomic.AddInt64(counter, 1)
	var once sync.Once
	return func() {
		// Capture the admitted generation's counter. StopTask may remove its map
		// entry; an old release must never decrement a subsequently-created one.
		once.Do(func() { atomic.AddInt64(counter, -1) })
	}, true
}

func (e *Engine) taskFileReadCount(taskID string) int64 {
	value, ok := e.taskReads.Load(taskID)
	if !ok {
		return 0
	}
	return atomic.LoadInt64(value.(*int64))
}
