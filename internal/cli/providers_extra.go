package cli

// Providers with their own wire register themselves in the provider registry
// when imported (see internal/provider/registry.go).
import (
	_ "github.com/umairge66/gocode/internal/provider/azure"
	_ "github.com/umairge66/gocode/internal/provider/bedrock"
	_ "github.com/umairge66/gocode/internal/provider/vertex"
)
