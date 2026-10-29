package store_test

import (
	"testing"

	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/internal/store/storetest"
)

func TestMemoryConformance(t *testing.T) {
	storetest.Run(t, func(*testing.T) store.Store { return store.NewMemory() })
}
