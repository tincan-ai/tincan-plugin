package main

import (
	"bytes"
	"context"
	"errors"
	"github.com/tincan-ai/tincan-plugin/internal/core"
	"github.com/tincan-ai/tincan-plugin/internal/e2ee"
	"sort"
)

func cloneRoomMembers(in map[string][]string) map[string][]string {
	out := map[string][]string{}
	for rid, ids := range in {
		out[rid] = append([]string{}, ids...)
	}
	return out
}
func loadInitialRoomPolicy(ctx context.Context, c Config, r *e2ee.Roster) error {
	value, err := rawCallContext(ctx, c, "GET", "/e2ee/room-policy", nil)
	if err != nil {
		return err
	}
	var policy core.EncryptionRoomPolicy
	if err = decodeValue(value, &policy); err != nil {
		return err
	}
	r.RoomMembers = map[string][]string{}
	r.RoomChannels = policy.Channels
	for rid, ids := range policy.Members {
		r.RoomMembers[rid] = []string{}
		for _, id := range ids {
			if _, ok := r.Device(id); ok {
				r.RoomMembers[rid] = append(r.RoomMembers[rid], id)
			}
		}
		sort.Strings(r.RoomMembers[rid])
	}
	if len(r.RoomMembers) == 0 {
		r.RoomMembers = nil
		r.RoomChannels = nil
	}
	return nil
}
func registerEncryptedRoom(ctx context.Context, c Config, st *cryptoState, rid string) error {
	old := st.ActiveRoster
	if old.RoomHas(rid, st.AgentID) {
		return nil
	}
	next := old
	next.RoomMembers = cloneRoomMembers(old.RoomMembers)
	if old.RoomMembers == nil {
		if err := loadInitialRoomPolicy(ctx, c, &next); err != nil {
			return err
		}
	} else {
		next.RoomMembers[rid] = []string{st.AgentID}
	}
	next.Epoch++
	next.Previous = old.Hash()
	_, err := changeMLSRoster(ctx, c, st, old, next, "", "")
	return err
}
func encryptedRoomMember(ctx context.Context, c Config, rid, agent string, present bool) (any, error) {
	var err error
	c, err = cryptoConfigForRoom(c, rid)
	if err != nil {
		return nil, err
	}
	if c.EncryptionRoom != "" {
		if present {
			return nil, errors.New("create a room invitation, then have the existing agent call encryption_join_room to establish fresh keys for this room")
		}
		return encryptionManage(ctx, c, "", "", agent)
	}

	return withCrypto(ctx, c, func(st *cryptoState) (any, error) {
		if err := ensureCrypto(ctx, c, st); err != nil {
			return nil, err
		}
		if !bytes.Equal(st.Identity.SigningKey[32:], st.Root) {
			return nil, errors.New("use the encryption creator’s local connection to change room membership")
		}
		old := st.ActiveRoster
		if old.RoomMembers == nil {
			next := old
			if err := loadInitialRoomPolicy(ctx, c, &next); err != nil {
				return nil, err
			}
			next.Epoch++
			next.Previous = old.Hash()
			if st.Protocol == 2 {
				if _, err := changeMLSRoster(ctx, c, st, old, next, "", ""); err != nil {
					return nil, err
				}
			} else {
				if err := next.Sign(st.Identity); err != nil {
					return nil, err
				}
				if _, err := rawCallContext(ctx, c, "POST", "/e2ee/roster", map[string]any{"roster": next}); err != nil {
					return nil, err
				}
			}
			old = next
		}
		if !old.RoomHas(rid, st.AgentID) {
			return nil, errors.New("join this room before managing its members")
		}
		if _, ok := old.Device(agent); !ok {
			return nil, errors.New("admit this device with an encrypted room invitation first")
		}
		if !present && agent == st.AgentID {
			return nil, errors.New("the encryption creator must remain in this room")
		}
		next := old
		next.RoomMembers = cloneRoomMembers(old.RoomMembers)
		ids := []string{}
		for _, id := range next.RoomMembers[rid] {
			if id != agent {
				ids = append(ids, id)
			}
		}
		if present {
			ids = append(ids, agent)
		}
		sort.Strings(ids)
		next.RoomMembers[rid] = ids
		next.Epoch++
		next.Previous = old.Hash()
		if st.Protocol != 2 {
			if err := next.Sign(st.Identity); err != nil {
				return nil, err
			}
			return rawCallContext(ctx, c, "POST", "/e2ee/roster", map[string]any{"roster": next})
		}
		return changeMLSRoster(ctx, c, st, old, next, "", "")
	})
}
