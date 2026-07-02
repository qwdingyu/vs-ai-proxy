package provider

import "embed"

// defaultModelSelectionFS carries the same built-in provider/model defaults as
// the reference C# proxy. User config loaded from disk is applied after these
// defaults and can override individual provider/model entries.
//
//go:embed model-selection/*.json
var defaultModelSelectionFS embed.FS
