// Package protocol defines the public, implementation-independent discovery contract.
package protocol

import (
	"fmt"
	"slices"
)

const (
	Path        = "/.well-known/tincan.json"
	Version     = "0.1"
	Profile     = "tincan-plugin/0.1"
	CoreProfile = "tincan-core/0.1"
)

// RequiredTools is the minimum remote MCP surface used by the plugin profile.
var RequiredTools = []string{"workspace_info", "rooms_list", "channels_list", "agents_list", "room_members", "room_create", "channel_create", "room_member_update", "message_send", "messages_search", "invite_create", "events_wait"}

// Descriptor advertises server support, not account permissions or host wake support.
// Unknown versions, profiles, capabilities and tools may be ignored by clients.
type Descriptor struct {
	Protocol     string   `json:"protocol"`
	Versions     []string `json:"versions"`
	Profiles     []string `json:"profiles"`
	Capabilities []string `json:"capabilities"`
	Tools        []string `json:"tools"`
}

func (d *Descriptor) Validate() error {
	if d.Protocol != "tincan" || !slices.Contains(d.Versions, Version) || !slices.Contains(d.Profiles, Profile) || !slices.Contains(d.Profiles, CoreProfile) {
		return fmt.Errorf("server does not support Tincan %s with profile %s", Version, Profile)
	}
	if d.Capabilities == nil || d.Tools == nil {
		return fmt.Errorf("Tincan discovery requires capabilities and tools arrays")
	}
	for _, tool := range RequiredTools {
		if !slices.Contains(d.Tools, tool) {
			return fmt.Errorf("Tincan plugin profile requires server tool %s", tool)
		}
	}
	return nil
}

func (d *Descriptor) Supports(capability string) bool {
	return d == nil || slices.Contains(d.Capabilities, capability)
}

func (d *Descriptor) SupportsTool(name string) bool {
	return d == nil || slices.Contains(d.Tools, name)
}
