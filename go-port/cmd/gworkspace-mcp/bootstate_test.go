package main

import (
	"sync"
	"testing"

	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/drive"
)

func TestBootCell_ZeroUntilPublished(t *testing.T) {
	var c bootCell
	if c.Ready() || c.LegacyToken() != "" || c.PasswordHash() != nil || c.PlrToken() != "" || c.Drive() != nil {
		t.Fatal("unpublished cell must read as not ready with zero values")
	}
	hash := "scrypt$1$1$1$a$b"
	client := &drive.Client{}
	c.Publish(&bootState{legacyToken: "legacy", passwordHash: &hash, plrToken: "plr", driveClient: client})
	if !c.Ready() || c.LegacyToken() != "legacy" || *c.PasswordHash() != hash || c.PlrToken() != "plr" || c.Drive() != client {
		t.Fatal("published values not visible")
	}
}

// Readers racing the publish: run with -race.
func TestBootCell_ConcurrentReadsDuringPublish(t *testing.T) {
	var c bootCell
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				if c.Ready() && c.LegacyToken() != "legacy" {
					t.Error("ready but token not visible")
					return
				}
			}
		}()
	}
	empty := ""
	c.Publish(&bootState{legacyToken: "legacy", passwordHash: &empty})
	wg.Wait()
}
