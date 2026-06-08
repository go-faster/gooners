package grafana

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-faster/gooners/internal/tools/mcputil"
)

type AddRowReq struct {
	DashboardID string `json:"dashboard_id" jsonschema:"The ID of the dashboard session"`
	Title       string `json:"title" jsonschema:"The title of the row"`
	Collapsed   bool   `json:"collapsed,omitempty" jsonschema:"Whether the row is collapsed"`
}

type AddRowRes struct {
	RowID string `json:"row_id"`
}

func addRowHandler(sm *SessionManager) mcp.ToolHandlerFor[AddRowReq, AddRowRes] {
	return func(_ context.Context, _ *mcp.CallToolRequest, args AddRowReq) (*mcp.CallToolResult, AddRowRes, error) {
		rowID := uuid.New().String()
		err := sm.Update(args.DashboardID, func(s *DashboardSession) error {
			rY := s.NextY
			s.Rows = append(s.Rows, &RowEntry{
				ID:        rowID,
				Title:     args.Title,
				Collapsed: args.Collapsed,
				Y:         rY,
				NextX:     0,
				NextY:     rY + 1,
			})
			s.NextY++
			return nil
		})
		if err != nil {
			return nil, AddRowRes{}, err
		}
		return nil, AddRowRes{RowID: rowID}, nil
	}
}

type UpdateRowReq struct {
	DashboardID string `json:"dashboard_id" jsonschema:"The ID of the dashboard session"`
	RowID       string `json:"row_id" jsonschema:"The ID of the row"`
	Title       string `json:"title,omitempty" jsonschema:"Optional new title"`
	Collapsed   *bool  `json:"collapsed,omitempty" jsonschema:"Optional collapsed state"`
}

func updateRowHandler(sm *SessionManager) mcp.ToolHandlerFor[UpdateRowReq, mcputil.SuccessResult] {
	return func(_ context.Context, _ *mcp.CallToolRequest, args UpdateRowReq) (*mcp.CallToolResult, mcputil.SuccessResult, error) {
		err := sm.Update(args.DashboardID, func(s *DashboardSession) error {
			r := s.findRow(args.RowID)
			if r == nil {
				return fmt.Errorf("row_id %s not found", args.RowID)
			}
			if args.Title != "" {
				r.Title = args.Title
			}
			if args.Collapsed != nil {
				r.Collapsed = *args.Collapsed
			}
			return nil
		})
		if err != nil {
			return nil, mcputil.SuccessResult{OK: false}, err
		}
		return nil, mcputil.SuccessResult{OK: true}, nil
	}
}

type DeleteRowReq struct {
	DashboardID string `json:"dashboard_id" jsonschema:"The ID of the dashboard session"`
	RowID       string `json:"row_id" jsonschema:"The ID of the row"`
	KeepPanels  bool   `json:"keep_panels,omitempty" jsonschema:"If true, panels inside the row are promoted to dashboard top-level instead of being discarded"`
}

func deleteRowHandler(sm *SessionManager) mcp.ToolHandlerFor[DeleteRowReq, mcputil.SuccessResult] {
	return func(_ context.Context, _ *mcp.CallToolRequest, args DeleteRowReq) (*mcp.CallToolResult, mcputil.SuccessResult, error) {
		err := sm.Update(args.DashboardID, func(s *DashboardSession) error {
			idx := s.findRowIndex(args.RowID)
			if idx < 0 {
				return fmt.Errorf("row_id %s not found", args.RowID)
			}
			r := s.Rows[idx]
			if args.KeepPanels {
				s.Panels = append(s.Panels, r.Panels...)
			}
			s.Rows = append(s.Rows[:idx], s.Rows[idx+1:]...)
			return nil
		})
		if err != nil {
			return nil, mcputil.SuccessResult{OK: false}, err
		}
		return nil, mcputil.SuccessResult{OK: true}, nil
	}
}

type MoveRowReq struct {
	DashboardID string `json:"dashboard_id" jsonschema:"The ID of the dashboard session"`
	RowID       string `json:"row_id" jsonschema:"The ID of the row to move"`
	BeforeRowID string `json:"before_row_id,omitempty" jsonschema:"Move the row before this row. Pass empty string to move to the end."`
}

func moveRowHandler(sm *SessionManager) mcp.ToolHandlerFor[MoveRowReq, mcputil.SuccessResult] {
	return func(_ context.Context, _ *mcp.CallToolRequest, args MoveRowReq) (*mcp.CallToolResult, mcputil.SuccessResult, error) {
		err := sm.Update(args.DashboardID, func(s *DashboardSession) error {
			srcIdx := s.findRowIndex(args.RowID)
			if srcIdx < 0 {
				return fmt.Errorf("row_id %s not found", args.RowID)
			}

			row := s.Rows[srcIdx]

			// Build a new slice without the source row.
			without := make([]*RowEntry, 0, len(s.Rows)-1)
			without = append(without, s.Rows[:srcIdx]...)
			without = append(without, s.Rows[srcIdx+1:]...)

			if args.BeforeRowID == "" {
				without = append(without, row)
				s.Rows = without
				return nil
			}

			dstIdx := -1
			for i, r := range without {
				if r.ID == args.BeforeRowID {
					dstIdx = i
					break
				}
			}
			if dstIdx < 0 {
				return fmt.Errorf("before_row_id %s not found", args.BeforeRowID)
			}

			// Insert at dstIdx.
			result := make([]*RowEntry, 0, len(without)+1)
			result = append(result, without[:dstIdx]...)
			result = append(result, row)
			result = append(result, without[dstIdx:]...)
			s.Rows = result
			return nil
		})
		if err != nil {
			return nil, mcputil.SuccessResult{OK: false}, err
		}
		return nil, mcputil.SuccessResult{OK: true}, nil
	}
}
