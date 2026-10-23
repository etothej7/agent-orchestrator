package coordination

import (
	"context"
	"sync"
)

type guardContextKey struct{}

// ReadLock keeps assignment changes out of a launch, report, or removal. Nested
// lifecycle calls share the same lease through ctx (RWMutex is not reentrant).
// Legacy stores without coordinator support do not need a gate.
func ReadLock(ctx context.Context, store any) (context.Context, func()) {
	provider, ok := store.(interface{ CoordinationMutex() *sync.RWMutex })
	if !ok {
		return ctx, func() {}
	}
	mu := provider.CoordinationMutex()
	if ctx.Value(guardContextKey{}) == mu {
		return ctx, func() {}
	}
	mu.RLock()
	return context.WithValue(ctx, guardContextKey{}, mu), mu.RUnlock
}
