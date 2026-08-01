package recording_test

import (
	"os"
	"testing"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
	"github.com/arena-trading-agent/arena-trading-agent/internal/recording"
)

func TestStoreWritesFrameAndObservation(t *testing.T) {
	store, err := recording.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.SaveFrame(protocol.Frame{
		ID: 4, Encoding: "png", Data: []byte("png"), CapturedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.SHA256 == "" {
		t.Fatal("missing SHA-256")
	}
	if err := store.SaveObservation(record, domain.Observation{
		FrameID: 4, State: domain.StateUnknown, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{record.Path, record.Path + ".json", record.Path + ".observation.json"} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing artifact %s: %v", path, err)
		}
	}
}
