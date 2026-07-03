package provider

import "embed"

// defaultModelSelectionFS carries the same built-in provider/model defaults as
// the reference C# proxy. User config loaded from disk is applied after these
// defaults and can override individual provider/model entries.
//
//go:embed model-selection/*.json
var defaultModelSelectionFS embed.FS

// defaultModelMetadataFS embeds the upstream model metadata seed copied from
// API-Switch. It is intentionally lower priority than model-selection so
// curated project defaults and user overrides keep winning.
//
//go:embed model-metadata/*.json
var defaultModelMetadataFS embed.FS
