package host

import (
	"context"
	"fmt"

	sdk "github.com/coder/acp-go-sdk"
	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/agent/acp"
)

func (m *ACPBackendManager) discoverSessionPages(ctx context.Context, conn *acp.AdapterConn, req sdk.ListSessionsRequest) ([]agent.SessionSnapshot, error) {
	sessions := make([]agent.SessionSnapshot, 0)
	seenCursors := map[string]bool{"": true}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := conn.Conn().ListSessions(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("acp %s: session/list: %w", m.profile.ID, err)
		}
		sessions = append(sessions, m.snapshots(page)...)
		if page.NextCursor == nil {
			return sessions, nil
		}
		if seenCursors[*page.NextCursor] {
			return nil, fmt.Errorf("acp %s: session/list returned an empty or repeated cursor", m.profile.ID)
		}
		seenCursors[*page.NextCursor] = true
		req.Cursor = page.NextCursor
	}
}
