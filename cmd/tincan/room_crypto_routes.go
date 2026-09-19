package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/tincan-ai/tincan-plugin/internal/core"
	"github.com/tincan-ai/tincan-plugin/internal/e2ee"
	"net/url"
	"os"
	"sort"
	"strings"
)

// Resolve only locally saved room identities. Server routing metadata can never
// select a new trust root or cause a plaintext fallback for an encrypted room.
func cryptoConfigForRoom(c Config, room string) (Config, error) {
	if room == "" {
		return c, nil
	}
	if path := c.CryptoRooms[room]; path != "" {
		c.CryptoPath = path
		c.EncryptionRoom = room
		return c, nil
	}
	var saved struct {
		RoomID string `json:"room_id"`
	}
	if b, err := os.ReadFile(c.CryptoPath); err == nil {
		_ = json.Unmarshal(b, &saved)
	}
	if saved.RoomID == room || saved.RoomID == "" && strings.HasPrefix(room, "rm_mls_") {
		c.EncryptionRoom = room
		return c, nil
	}
	if strings.HasPrefix(room, "rm_mls_") {
		return c, errors.New("this connection has no keys for this room; join it with a room invitation")
	}
	return c, nil // Legacy workspace encryption.
}
func cryptoConfigForChannel(c Config, channel string) (Config, error) {
	return cryptoConfigForRoom(c, e2ee.RoomForChannel(channel))
}

func routeCryptoCall(c Config, path string, value any) (Config, error) {
	u, err := url.Parse(path)
	if err != nil {
		return c, err
	}
	channel, room := u.Query().Get("channel_id"), u.Query().Get("room_id")
	if value != nil {
		var args struct {
			Channel string `json:"channel_id"`
			Room    string `json:"room_id"`
		}
		if err = decodeValue(value, &args); err != nil {
			return c, err
		}
		if args.Channel != "" {
			channel = args.Channel
		}
		if args.Room != "" {
			room = args.Room
		}
	}
	if channel != "" {
		return cryptoConfigForChannel(c, channel)
	}
	return cryptoConfigForRoom(c, room)
}

func decryptRoomMessage(ctx context.Context, c Config, m *core.Message) error {
	if m.Encrypted == nil {
		if strings.HasPrefix(m.ChannelID, "ch_plain_") {
			return nil
		}
		_, err := withCrypto(ctx, c, func(st *cryptoState) (any, error) {
			if plainCryptoChannel(st, m.ChannelID) {
				return nil, nil
			}
			return nil, errors.New("plaintext message rejected in encrypted room")
		})
		return err
	}
	scoped, err := cryptoConfigForChannel(c, m.ChannelID)
	if err != nil {
		return err
	}
	_, err = withCrypto(ctx, scoped, func(st *cryptoState) (any, error) {
		if err := ensureCrypto(ctx, scoped, st); err != nil {
			return nil, err
		}
		return nil, decryptMessage(ctx, scoped, st, m)
	})
	return err
}
func openRoomFile(ctx context.Context, c Config, id, channel string, data []byte) (core.Attachment, []byte, error) {
	var envelope e2ee.Envelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return core.Attachment{}, nil, err
	}
	scoped, err := cryptoConfigForChannel(c, envelope.ChannelID)
	if err != nil {
		return core.Attachment{}, nil, err
	}
	var attachment core.Attachment
	var plain []byte
	_, err = withCrypto(ctx, scoped, func(st *cryptoState) (any, error) {
		if err := ensureCrypto(ctx, scoped, st); err != nil {
			return nil, err
		}
		var e error
		attachment, plain, e = openCryptoFile(ctx, scoped, st, id, channel, data)
		return nil, e
	})
	return attachment, plain, err
}

// Join another room without creating another workspace runtime. Its fresh
// identity and one-use key package are kept independently of all other rooms.
func joinIndependentRoom(ctx context.Context, c *Config, raw string, persist func() error) (any, error) {
	clean, root, err := cryptoJoinAddress(raw)
	if err != nil {
		return nil, err
	}
	if len(root) == 0 {
		return nil, errors.New("use the encrypted invitation from this room's creator")
	}
	server, invite, err := connectAddress(clean, c.Server)
	if err != nil {
		return nil, err
	}
	if server != c.Server {
		return nil, errors.New("invitation belongs to another server")
	}
	candidate := *c
	base := c.CryptoPath
	if base == "" {
		base = c.ConnectionPath
		if base == "" {
			base = configPath()
		}
		base += ".e2ee.json"
	}
	candidate.CryptoPath = base + ".rooms/" + core.ID("device_") + ".json"
	candidate.EncryptionRoom = ""
	if err = newCrypto(candidate.CryptoPath, c.Server, root); err != nil {
		return nil, err
	}
	if err = prepareAdmissionJoin(ctx, candidate, raw); err != nil {
		return nil, err
	}
	value, err := rawCallContext(ctx, *c, "GET", "/me", nil)
	if err != nil {
		return nil, err
	}
	var me struct {
		Agent core.Agent `json:"agent"`
	}
	if err = decodeValue(value, &me); err != nil {
		return nil, err
	}
	input := map[string]any{"invite": invite, "name": me.Agent.Name, "profile": ""}
	if err = cryptoConnectionInput(ctx, candidate, invite, input); err != nil {
		return nil, err
	}
	value, err = rawCallContext(ctx, candidate, "POST", "/e2ee/join-existing", input)
	if err != nil {
		return nil, err
	}
	var pending struct {
		RoomID  string `json:"room_id"`
		Receipt string `json:"receipt"`
	}
	if err = decodeValue(value, &pending); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(pending.RoomID, "rm_mls_") || pending.Receipt == "" {
		return nil, errors.New("incomplete room admission response")
	}
	_, err = withCrypto(ctx, candidate, func(st *cryptoState) (any, error) {
		st.RoomID = pending.RoomID
		st.RoomJoinReceipt = pending.Receipt
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	if c.CryptoRooms == nil {
		c.CryptoRooms = map[string]string{}
	}
	c.CryptoRooms[pending.RoomID] = candidate.CryptoPath
	if c.CryptoPath == "" {
		c.CryptoPath = candidate.CryptoPath
	}
	if err = persist(); err != nil {
		return nil, err
	}
	// The receipt remains private; resume by using this connection and room_id.
	result := value.(map[string]any)
	delete(result, "receipt")
	result["next"] = "The room admission is saved. The creator can approve this device; then use this same connection and room_id."
	return result, nil
}

func managedCryptoConfigs(c Config) []Config {
	paths := map[string]string{c.CryptoPath: ""}
	for room, path := range c.CryptoRooms {
		paths[path] = room
	}
	keys := []string{}
	for path := range paths {
		keys = append(keys, path)
	}
	sort.Strings(keys)
	configs := []Config{}
	for _, path := range keys {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var st cryptoState
		if json.Unmarshal(b, &st) != nil || len(st.Identity.SigningKey) != 64 || !bytes.Equal(st.Identity.SigningKey[32:], st.Root) {
			continue
		}
		scoped := c
		scoped.CryptoPath = path
		scoped.EncryptionRoom = st.RoomID
		scoped.CryptoRooms = nil
		configs = append(configs, scoped)
	}
	return configs
}
