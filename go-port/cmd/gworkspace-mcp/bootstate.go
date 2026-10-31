package main

import (
	"sync/atomic"

	"github.com/sarvinshrivastava/gworkspace-mcp/go-port/internal/drive"
)

// bootState is everything boot learns from the vault. It is published as a
// whole once the vault has answered, while both listeners already serve: a
// handler sees either none of it or all of it, never a half-written string.
type bootState struct {
	legacyToken  string
	passwordHash *string
	plrToken     string
	driveClient  *drive.Client
}

// bootCell holds the published bootState and the ready flag. Every getter
// returns the zero value until Publish.
type bootCell struct {
	p     atomic.Pointer[bootState]
	ready atomic.Bool
}

// Publish makes the vault-derived state visible, before the boot probe: the
// dashboard login and /internal/* don't depend on Google, so they need not
// wait for it (as in the TS).
func (c *bootCell) Publish(s *bootState) { c.p.Store(s) }

// MarkReady opens /mcp and /healthz, after the boot probe has run.
func (c *bootCell) MarkReady() { c.ready.Store(true) }

// Ready: published and probed.
func (c *bootCell) Ready() bool { return c.ready.Load() }

// LegacyToken is the vault's GWORKSPACE_MCP_TOKEN, "" until published.
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

// PlrToken is the usable GWORKSPACE_PLR_TOKEN, "" until published.
func (c *bootCell) PlrToken() string {
	if s := c.p.Load(); s != nil {
		return s.plrToken
	}
	return ""
}

// Drive is the Google Drive client, nil until published.
func (c *bootCell) Drive() *drive.Client {
	if s := c.p.Load(); s != nil {
		return s.driveClient
	}
	return nil
}
