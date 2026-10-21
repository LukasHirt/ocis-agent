package tools

// All returns a fresh instance of every tool of this version, so callers can
// register them next to their own.
func All() []Tool {
	return []Tool{
		NewListSpaces(), NewList(), NewStat(), NewReadText(), NewRecentChanges(),
		NewCreateFolder(), NewWriteNew(), NewOverwrite(), NewMove(), NewCopy(), NewDelete(),
		NewMemoryList(), NewMemoryRead(), NewMemoryWrite(),
	}
}

// NewDefault returns the registry of all tools of this version.
func NewDefault() (*Registry, error) { return NewRegistry(All()...) }
