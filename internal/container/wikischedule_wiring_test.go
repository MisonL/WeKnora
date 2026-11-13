package container

import (
	"testing"

	"go.uber.org/dig"

	"github.com/Tencent/WeKnora/internal/application/wikischedule"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// A constructor whose parameter is not registered leaves dig searching for a
// provider and panics the whole container at startup, which only shows up on a
// real boot. Build the scheduler's dependency graph in isolation so the
// parameter type cannot silently drift away from what the container provides.
func TestWikiSchedulerWiringResolvesFromRepository(t *testing.T) {
	c := dig.New()
	if err := c.Provide(wikischedule.NewSchedulerFromRepository); err != nil {
		t.Fatalf("provide: %v", err)
	}
	if err := c.Provide(func() interfaces.KnowledgeBaseRepository { return nil }); err != nil {
		t.Fatalf("provide repository: %v", err)
	}
	if err := c.Provide(func() interfaces.TaskEnqueuer { return nil }); err != nil {
		t.Fatalf("provide enqueuer: %v", err)
	}
	if err := c.Invoke(func(*wikischedule.Scheduler) {}); err != nil {
		t.Fatalf("scheduler could not be built: %v", err)
	}
}
