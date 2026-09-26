package auth

import (
	"sync"
	"testing"
	"time"
)

func TestGeneration(t *testing.T) {
	g0, ch := Watch()
	if g0 != Generation() {
		t.Fatalf("Watch generation %d != Generation() %d", g0, Generation())
	}
	select {
	case <-ch:
		t.Fatal("Watch channel closed before any Bump")
	default:
	}

	if g := Bump(); g != g0+1 {
		t.Errorf("Bump() = %d, want %d", g, g0+1)
	}
	if Generation() != g0+1 {
		t.Errorf("Generation() = %d after Bump, want %d", Generation(), g0+1)
	}
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("Watch channel not closed by Bump")
	}

	g1, ch1 := Watch()
	if g1 != g0+1 {
		t.Errorf("Watch after Bump = %d, want %d", g1, g0+1)
	}
	select {
	case <-ch1:
		t.Fatal("a fresh Watch channel is already closed")
	default:
	}
}

func TestGenerationConcurrentBumps(t *testing.T) {
	g0, ch := Watch()
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			Bump()
			Watch()
		}()
	}
	wg.Wait()
	if got := Generation(); got != g0+50 {
		t.Errorf("Generation() = %d after 50 concurrent bumps, want %d", got, g0+50)
	}
	select {
	case <-ch:
	default:
		t.Error("Watch channel still open after bumps")
	}
}
