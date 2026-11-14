package agentruntime

import (
	"context"
	"time"
)

// armEffect sets a timer that, when it fires, enqueues the effect as durable
// work — the "scheduled effect" invocation trigger (Runtime §2).
func (rt *Runtime) armEffect(threadID string, e ScheduledEffect) {
	delay := time.Until(time.UnixMicro(e.RunAt))
	if delay < 0 {
		delay = 0
	}
	key := threadID + "/" + e.ID
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.closed {
		return
	}
	if old, ok := rt.timers[key]; ok {
		old.Stop()
	}
	rt.timers[key] = time.AfterFunc(delay, func() {
		rt.mu.Lock()
		delete(rt.timers, key)
		rt.mu.Unlock()
		if err := rt.enqueue(context.Background(), threadID, QueueItem{Kind: QueueEffect, EffectID: e.ID}); err != nil {
			rt.cfg.Logger.Warn("enqueue effect", "thread", threadID, "effect", e.ID, "err", err)
		}
	})
}

// runEffect executes a due effect inside the thread's serialized execution.
func (rt *Runtime) runEffect(ctx context.Context, s *ThreadState, effectID string) error {
	effects, err := rt.store.ListEffects(ctx, s.threadID)
	if err != nil {
		return err
	}
	var e *ScheduledEffect
	for i := range effects {
		if effects[i].ID == effectID {
			e = &effects[i]
		}
	}
	if e == nil {
		return nil // removed before it ran
	}
	def := rt.reg.Effect(e.Name)
	if def == nil {
		_, _ = rt.store.DeleteEffect(ctx, s.threadID, effectID)
		return errf("effect %q not defined; dropped", e.Name)
	}
	// The durable record is removed only after the effect succeeds, so a
	// crash mid-run re-arms it on Start (at-least-once).
	runErr := func() (err error) {
		defer func() {
			if p := recover(); p != nil {
				err = errf("effect %q panicked: %v", e.Name, p)
			}
		}()
		return def.Execute(ctx, s, e.Args)
	}()
	if runErr == nil {
		_, err := rt.store.DeleteEffect(context.Background(), s.threadID, effectID)
		return err
	}
	e.Attempts++
	if e.Attempts >= maxEffectAttempts {
		_, _ = rt.store.DeleteEffect(context.Background(), s.threadID, effectID)
		return errf("effect %q failed %d times; dropped: %v", e.Name, e.Attempts, runErr)
	}
	e.RunAt = time.Now().Add(time.Duration(e.Attempts) * effectRetryBackoff).UnixMicro()
	if err := rt.store.PutEffect(context.Background(), s.threadID, *e); err != nil {
		return err
	}
	rt.armEffect(s.threadID, *e)
	return runErr
}

const maxEffectAttempts = 3

var effectRetryBackoff = time.Second
