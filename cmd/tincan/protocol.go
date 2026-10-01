package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tincan-ai/tincan-plugin/internal/protocol"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Discovery is anonymous and cannot redirect credentials or select new endpoints.
// A 404 is the only legacy fallback. Invalid documents and transient errors fail closed.
func discoverProtocol(ctx context.Context, server string) (*protocol.Descriptor, error) {
	if err := validateServer(server); err != nil {
		return nil, err
	}
	u, _ := url.Parse(server)
	if u.Path != "" && u.Path != "/" {
		return nil, errors.New("Tincan protocol servers must be origins without a path")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(server, "/")+protocol.Path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return nil, errors.New("could not discover Tincan server capabilities; check the server address and connectivity")
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Tincan discovery returned HTTP %d", res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 128*1024+1))
	if err != nil || len(data) > 128*1024 {
		return nil, errors.New("invalid or oversized Tincan discovery document")
	}
	var d protocol.Descriptor
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, errors.New("invalid Tincan discovery document")
	}
	if err := d.Validate(); err != nil {
		return nil, err
	}
	return &d, nil
}

func (b *pluginBroker) refreshProtocol(ctx context.Context, c *pluginConnection) error {
	d, err := discoverProtocol(ctx, c.Config.Server)
	if err != nil {
		return err
	}
	if c.Protocol != nil && d == nil {
		return errors.New("server no longer advertises the negotiated Tincan protocol; refusing a legacy downgrade")
	}
	if c.Config.CryptoPath != "" && !d.Supports("e2ee") {
		return errors.New("this server does not support encrypted rooms")
	}
	c.Protocol = d
	return nil
}

func checkServerTool(c *pluginConnection, name string, args map[string]any) error {
	if isLocalCryptoTool(name) || (name == "room_create" && args["e2ee"] == true) {
		if !c.Protocol.Supports("e2ee") {
			return errors.New("this server does not support encrypted rooms")
		}
		if isLocalCryptoTool(name) {
			return nil
		}
	}
	if !c.Protocol.SupportsTool(name) {
		return fmt.Errorf("this server does not support %s; inspect tincan_status for available server tools", name)
	}
	return nil
}
