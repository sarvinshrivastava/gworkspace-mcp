package main

import (
	"sync/atomic"

	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/drive"
)

// bootState is everything boot learns from the vault, published once both
// listeners already serve. Published as a whole: a handler sees either none
// of it (not ready) or all of it, never a half-written string.
type bootState struct {
	legacyToken  string
	passwordHash *string
	plrToken     string
	driveClient  *drive.Client
}

type bootCell struct{ p atomic.Pointer[bootState] }

func (c *bootCell) Publish(s *bootState) { c.p.Store(s) }

// Ready: the vault has answered and the boot probe has run.
func (c *bootCell) Ready() bool { return c.p.Load() != nil }

func (c *bootCell) LegacyToken() string {
	if s := c.p.Load(); s != nil {
		return s.legacyToken
	}
	return ""
}

// PasswordHash stays nil until publish: nil is the admin "starting" state.
func (c *bootCell) PasswordHash() *string {
	if s := c.p.Load(); s != nil {
		return s.passwordHash
	}
	return nil
}

func (c *bootCell) PlrToken() string {
	if s := c.p.Load(); s != nil {
		return s.plrToken
	}
	return ""
}

func (c *bootCell) Drive() *drive.Client {
	if s := c.p.Load(); s != nil {
		return s.driveClient
	}
	return nil
}
