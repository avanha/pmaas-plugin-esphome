// Package entities holds the entities this plugin registers with the container, and the stubs it
// hands out for them. An entity's state is owned by the plugin goroutine; a stub is how any other
// goroutine reads it, by hopping onto the plugin goroutine.
package entities

import (
	"sync/atomic"

	spi "github.com/avanha/pmaas-spi"
	spicommon "github.com/avanha/pmaas-spi/common"
)

// link connects a stub to its entity. Closing it makes every later call through the stub fail
// fast instead of reaching an entity that has been deregistered.
type link[T any] struct {
	wrapper atomic.Pointer[spicommon.ThreadSafeEntityWrapper[T]]
}

func newLink[T any](container spi.IPMAASContainer, target T) *link[T] {
	l := &link[T]{}
	l.wrapper.Store(&spicommon.ThreadSafeEntityWrapper[T]{Container: container, Entity: target})

	return l
}

// close is idempotent.
func (l *link[T]) close() {
	l.wrapper.Store(nil)
}

// call runs f against the entity on the plugin goroutine and returns its result. It panics if the
// link has been closed, like the other plugins' stubs.
func call[T any, V any](l *link[T], f func(target T) V) V {
	return spicommon.ThreadSafeEntityWrapperExecValueFunc(l.wrapper.Load(), f)
}
