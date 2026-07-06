package alertmanager

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	receiverops "github.com/prometheus/alertmanager/api/v2/client/receiver"

	"github.com/go-faster/gooners/internal/tools/mcputil"
)

// ReceiverSummary is a compact, context-friendly view of a Receiver.
type ReceiverSummary struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
}

type ListReceiversReq struct{}

type ListReceiversRes struct {
	Receivers []ReceiverSummary `json:"receivers"`
}

func listReceiversHandler(c *Client) mcp.ToolHandlerFor[ListReceiversReq, ListReceiversRes] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ ListReceiversReq) (*mcp.CallToolResult, ListReceiversRes, error) {
		res, err := c.am.Receiver.GetReceivers(receiverops.NewGetReceiversParams().WithContext(ctx))
		if err != nil {
			return nil, ListReceiversRes{}, fmt.Errorf("get receivers: %w", err)
		}

		receivers := make([]ReceiverSummary, 0, len(res.Payload))
		for _, r := range res.Payload {
			if r == nil || r.Name == nil {
				continue
			}
			receivers = append(receivers, ReceiverSummary{
				Name:   *r.Name,
				Labels: map[string]string(r.Labels),
			})
		}
		return nil, ListReceiversRes{Receivers: receivers}, nil
	}
}

func registerReceiverTools(s *mcp.Server, c *Client) {
	mcputil.Register(s, mcputil.ToolDef{
		Name:        "list_receivers",
		Description: "Lists configured Alertmanager notification receivers and their label sets.",
		Flags:       mcputil.ReadOnly,
	}, listReceiversHandler(c))
}
