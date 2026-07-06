package alertmanager

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	generalops "github.com/prometheus/alertmanager/api/v2/client/general"

	"github.com/go-faster/gooners/internal/tools/mcputil"
)

// PeerSummary is a summary of a cluster peer.
type PeerSummary struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

// ClusterStatusSummary is a summary of cluster status.
type ClusterStatusSummary struct {
	Name   string        `json:"name,omitempty"`
	Status string        `json:"status,omitempty"` // ready, settling, disabled
	Peers  []PeerSummary `json:"peers,omitempty"`
}

// StatusResult is the response from get_status.
type StatusResult struct {
	Version    string               `json:"version,omitempty"`
	Revision   string               `json:"revision,omitempty"`
	GoVersion  string               `json:"go_version,omitempty"`
	Uptime     string               `json:"uptime,omitempty"` // RFC3339
	Cluster    ClusterStatusSummary `json:"cluster"`
	ConfigYAML string               `json:"config_yaml,omitempty"`
}

type GetStatusReq struct{}

func getStatusHandler(c *Client) mcp.ToolHandlerFor[GetStatusReq, StatusResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ GetStatusReq) (*mcp.CallToolResult, StatusResult, error) {
		res, err := c.am.General.GetStatus(generalops.NewGetStatusParams().WithContext(ctx))
		if err != nil {
			return nil, StatusResult{}, fmt.Errorf("get status: %w", err)
		}

		result := StatusResult{}
		payload := res.Payload
		if payload == nil {
			return nil, result, nil
		}

		// Version info
		if payload.VersionInfo != nil {
			if payload.VersionInfo.Version != nil {
				result.Version = *payload.VersionInfo.Version
			}
			if payload.VersionInfo.Revision != nil {
				result.Revision = *payload.VersionInfo.Revision
			}
			if payload.VersionInfo.GoVersion != nil {
				result.GoVersion = *payload.VersionInfo.GoVersion
			}
		}

		// Uptime
		if payload.Uptime != nil {
			result.Uptime = time.Time(*payload.Uptime).Format(time.RFC3339)
		}

		// Cluster status
		if payload.Cluster != nil {
			result.Cluster.Name = payload.Cluster.Name
			if payload.Cluster.Status != nil {
				result.Cluster.Status = *payload.Cluster.Status
			}
			for _, p := range payload.Cluster.Peers {
				if p == nil {
					continue
				}
				peer := PeerSummary{}
				if p.Name != nil {
					peer.Name = *p.Name
				}
				if p.Address != nil {
					peer.Address = *p.Address
				}
				result.Cluster.Peers = append(result.Cluster.Peers, peer)
			}
		}

		// Config
		if payload.Config != nil && payload.Config.Original != nil {
			result.ConfigYAML = *payload.Config.Original
		}

		return nil, result, nil
	}
}

func registerStatusTools(s *mcp.Server, c *Client) {
	mcputil.Register(s, mcputil.ToolDef{
		Name:        "get_status",
		Description: "Returns Alertmanager cluster status, version info, uptime, and the running configuration.",
		Flags:       mcputil.ReadOnly,
	}, getStatusHandler(c))
}
