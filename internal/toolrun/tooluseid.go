package toolrun

import "context"

// ToolUseIDFrom returns the id of the tool_use being dispatched on ctx ("" when
// none). A PermissionPrompter reads it to name the call it is asked about.
func ToolUseIDFrom(ctx context.Context) string { return toolUseIDFrom(ctx) }
