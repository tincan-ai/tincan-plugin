package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Host-neutral user/controller surface. JSON comes from stdin, never channel
// messages. It uses the live owner's authenticated bridge where available.
func inboxControl(args []string, input io.Reader, output io.Writer) error {
	f := flag.NewFlagSet("inbox-control", flag.ContinueOnError)
	connection := f.String("connection", "", "Private plugin/sidecar connection handle")
	directory := f.String("state-dir", os.Getenv("TINCAN_STATE_DIR"), "Private connection directory")
	action := f.String("action", "requests", "requests, decide, policy_set, or plan")
	senders := f.String("allow-senders", os.Getenv("TINCAN_ALLOW_SENDERS"), "Standalone CLI identity sender scope")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	names := map[string]string{"requests": "inbox_requests", "decide": "inbox_decide", "policy_set": "inbox_policy_set", "plan": "inbox_plan"}
	name, ok := names[*action]
	if !ok {
		return errors.New("unsupported controller action")
	}
	params := map[string]any{}
	if *action != "requests" {
		if err := json.NewDecoder(io.LimitReader(input, 128<<10)).Decode(&params); err != nil {
			return err
		}
	}
	var server *mcp.Server
	if *connection != "" {
		root, err := runtimeStateDirectory(*directory)
		if err != nil {
			return err
		}
		b := &pluginBroker{root: root, host: "controller"}
		defer b.close()
		server = b.serverWithTools(nil)
		params["connection"] = *connection
	} else {
		i, err := openInbox(load(), *senders)
		if err != nil {
			return err
		}
		defer i.close()
		server = mcp.NewServer(&mcp.Implementation{Name: "tincan-controller", Version: version}, nil)
		addRequestTools(server, func(string) (*inbox, error) { return i, nil })
	}
	a, z := mcp.NewInMemoryTransports()
	ss, err := server.Connect(context.Background(), a, nil)
	if err != nil {
		return err
	}
	defer ss.Close()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "tincan-controller", Version: version}, nil).Connect(context.Background(), z, nil)
	if err != nil {
		return err
	}
	defer client.Close()
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: params})
	if err != nil {
		return err
	}
	if result.IsError {
		data, _ := json.Marshal(result.Content)
		return errors.New(string(data))
	}
	return json.NewEncoder(output).Encode(result.StructuredContent)
}
