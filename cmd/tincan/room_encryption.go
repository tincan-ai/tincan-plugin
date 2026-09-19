package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/tincan-ai/tincan-plugin/internal/core"
	"github.com/tincan-ai/tincan-plugin/internal/e2ee"
	"os"
	"path/filepath"
	"strings"
)

// A running listener keeps its transport identity while picking up encryption
// activated by a room-creation tool in the same local connection.
func currentCryptoConfig(c Config) Config {
	if c.ConnectionPath == "" {
		return c
	}
	data, err := os.ReadFile(c.ConnectionPath)
	if err != nil {
		return c
	}
	var saved pluginConnection
	if json.Unmarshal(data, &saved) == nil && saved.Config.Token == c.Token && saved.Config.Server == c.Server && saved.Config.CryptoPath != "" {
		if c.EncryptionRoom == "" {
			c.CryptoPath = saved.Config.CryptoPath
		}
		c.CryptoRooms = saved.Config.CryptoRooms
	}
	return c
}

func plainCryptoChannel(st *cryptoState, cid string) bool {
	if strings.HasPrefix(cid, "ch_plain_") {
		return true
	}
	for _, id := range append(append([]string{}, st.ActiveRoster.PlainChannels...), st.InitialPlainChannels...) {
		if cid == id {
			return true
		}
	}
	return false
}

func requirePlainChannel(ctx context.Context, c Config, cid string) error {
	if cid == "" {
		return nil
	}
	v, err := rawCallContext(ctx, c, "GET", "/channels", nil)
	if err != nil {
		return err
	}
	var channels []core.Channel
	if err = decodeValue(v, &channels); err != nil {
		return err
	}
	for _, ch := range channels {
		if ch.ID == cid {
			if ch.EncryptionMode == "e2ee" {
				return errors.New("this room requires your local encryption keys; no plaintext was sent")
			}
			return nil
		}
	}
	return errors.New("channel unavailable")
}

type pendingRoomCreation struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
}

func createLocalEncryptedRoom(ctx context.Context, c *Config, path, name string, persist func() error) (any, error) {
	// Save the creation identity before HTTP so uncertain responses are retryable.
	if c.PendingEncryptedRoom == nil {
		pending := &pendingRoomCreation{ID: core.ID("rm_mls_"), Name: name, Path: path + ".rooms/" + core.ID("device_") + ".json"}
		if err := newCrypto(pending.Path, c.Server, nil); err != nil {
			return nil, err
		}
		c.PendingEncryptedRoom = pending
		if err := persist(); err != nil {
			return nil, err
		}
	}
	pending := c.PendingEncryptedRoom
	if pending.Name != name {
		return nil, errors.New("finish the pending encrypted room creation by retrying its original name: " + pending.Name)
	}
	candidate := *c
	candidate.CryptoPath = pending.Path
	candidate.EncryptionRoom = ""
	input := map[string]any{"name": name, "room_id": pending.ID}
	if err := cryptoConnectionInput(ctx, candidate, "", input); err != nil {
		return nil, err
	}
	v, err := rawCallContext(ctx, candidate, "POST", "/e2ee/rooms", input)
	if err != nil {
		if strings.HasPrefix(err.Error(), "HTTP 4") {
			c.PendingEncryptedRoom = nil
			if saveErr := persist(); saveErr != nil {
				return nil, saveErr
			}
		}
		return nil, err
	}
	var room core.Room
	if err = decodeValue(v, &room); err != nil {
		return nil, err
	}
	if room.ID != pending.ID {
		return nil, errors.New("room creation identity changed")
	}
	candidate.EncryptionRoom = room.ID
	c.PendingEncryptedRoom = nil
	if c.CryptoRooms == nil {
		c.CryptoRooms = map[string]string{}
	}
	c.CryptoRooms[room.ID] = candidate.CryptoPath
	if c.CryptoPath == "" {
		c.CryptoPath = candidate.CryptoPath
	}
	// Save the route before initialization so interrupted setup resumes the same group.
	if err = persist(); err != nil {
		return nil, err
	}
	_, err = withCrypto(ctx, candidate, func(st *cryptoState) (any, error) {
		st.RoomID = room.ID
		value, e := rawCallContext(ctx, candidate, "GET", "/channels", nil)
		if e != nil {
			return nil, e
		}
		var channels []core.Channel
		if e = decodeValue(value, &channels); e != nil {
			return nil, e
		}
		for _, ch := range channels {
			if ch.EncryptionMode == "standard" && e2ee.RoomForChannel(ch.ID) == "" && !strings.HasPrefix(ch.ID, "ch_e2ee_") {
				st.InitialPlainChannels = append(st.InitialPlainChannels, ch.ID)
			}
		}
		if err := privateJSON(candidate.CryptoPath, st); err != nil {
			return nil, err
		}
		return nil, ensureCrypto(ctx, candidate, st)
	})
	if err != nil {
		return map[string]any{"room": v, "setup_error": err.Error(), "next": "Resume this saved connection to finish the room's encryption setup."}, nil
	}
	return v, nil
}

func (b *pluginBroker) createEncryptedRoom(ctx context.Context, c *pluginConnection, name string) (any, error) {
	return createLocalEncryptedRoom(ctx, &c.Config, filepath.Join(b.root, c.Handle+".e2ee.json"), name, func() error { return b.save(c) })
}
