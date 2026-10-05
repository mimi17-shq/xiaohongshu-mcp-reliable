package main

import (
	"context"
	"testing"
	"time"
)

func TestBrowserSlotIsSingleAndCancellationAware(t *testing.T) {
	release, err := acquireBrowserSlot(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := acquireBrowserSlot(waitCtx); err == nil {
		t.Fatal("second browser slot should wait and honor context cancellation")
	}

	// Release is idempotent, which protects against overlapping cleanup paths.
	release()
	release()

	thirdCtx, thirdCancel := context.WithTimeout(context.Background(), time.Second)
	defer thirdCancel()
	thirdRelease, err := acquireBrowserSlot(thirdCtx)
	if err != nil {
		t.Fatalf("slot was not reusable after release: %v", err)
	}
	thirdRelease()
}
